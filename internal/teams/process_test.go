package teams

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The helper process owns its database connection. Crash mode exits only after
// the real write transaction has dirtied/spilled pages, bypassing all defers.
func TestCatalogProcessHelper(t *testing.T) {
	mode := os.Getenv("DELEGATE_TEAMS_TEST_MODE")
	if mode == "" {
		return
	}
	catalog := New(os.Getenv("DELEGATE_TEAMS_TEST_PATH"), nil)
	switch mode {
	case "register":
		id := os.Getenv("DELEGATE_TEAMS_TEST_TEAM")
		if _, _, err := catalog.Register(t.Context(), userRef(id), testTeam(id)); err != nil {
			t.Fatal(err)
		}
	case "crash":
		definition := testTeam("review")
		definition.Purpose = strings.Repeat("uncommitted ", 20000)
		data, err := catalog.definition(userRef("review"), definition)
		if err != nil {
			t.Fatal(err)
		}
		err = catalog.transact(t.Context(), catalogWrite, func(ctx context.Context, tx *sql.Tx, _ string) error {
			if _, pragmaErr := tx.ExecContext(ctx, "PRAGMA cache_size = 5"); pragmaErr != nil {
				return pragmaErr
			}
			if _, updateErr := tx.ExecContext(ctx, "UPDATE teams SET revision = 2, definition = ? WHERE team_id = 'review'", data); updateErr != nil {
				return updateErr
			}
			os.Exit(0)
			return nil
		})
		t.Fatalf("crash helper returned instead of exiting inside transaction: %v", err)
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func catalogProcess(t *testing.T, catalog *Catalog, mode, team string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestCatalogProcessHelper$", "-test.timeout=20s")
	command.Env = []string{
		"DELEGATE_TEAMS_TEST_MODE=" + mode,
		"DELEGATE_TEAMS_TEST_PATH=" + catalog.path,
		"DELEGATE_TEAMS_TEST_TEAM=" + team,
	}
	return command
}

func TestSeparateProcessWritersInitializeOneCatalog(t *testing.T) {
	catalog := testCatalog(t)
	const writers = 4
	type processResult struct {
		output []byte
		err    error
	}
	results := make(chan processResult, writers)
	commands := make([]*exec.Cmd, writers)
	for i := range writers {
		commands[i] = catalogProcess(t, catalog, "register", fmt.Sprintf("team-%d", i))
	}
	for _, command := range commands {
		go func() {
			output, err := command.CombinedOutput()
			results <- processResult{output: output, err: err}
		}()
	}
	for range writers {
		result := <-results
		if result.err != nil {
			t.Errorf("writer process: %v: %s", result.err, result.output)
		}
	}
	listing, err := catalog.List(t.Context(), nil)
	if err != nil || len(listing.Records) != writers || listing.CatalogID == "" {
		t.Fatalf("separate-process writes lost: %+v %v", listing, err)
	}
}

func TestCrashRollbackRequiresWriterRecoveryWithoutReadRepair(t *testing.T) {
	catalog := testCatalog(t)
	ref := userRef("review")
	_, id := registerTeam(t, catalog, ref)
	command := catalogProcess(t, catalog, "crash", "")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("crash fixture failed: %v: %s", err, output)
	}
	journalPath := catalog.path + "-journal"
	journal := readFile(t, journalPath)
	database := readFile(t, catalog.path)
	if len(journal) < 512 || bytes.Equal(journal[:8], make([]byte, 8)) {
		t.Fatal("crash fixture did not leave a hot rollback journal")
	}
	if _, _, err := catalog.Lookup(t.Context(), ref); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("passive reader accepted hot-journal database: %v", err)
	}
	if !bytes.Equal(journal, readFile(t, journalPath)) || !bytes.Equal(database, readFile(t, catalog.path)) {
		t.Fatal("passive reader repaired crash state")
	}
	_, recoveredID := registerTeam(t, New(catalog.path, nil), userRef("after-recovery"))
	got, gotID, err := catalog.Lookup(t.Context(), ref)
	if err != nil || got.Revision != 1 || got.Definition.Purpose != testTeam("review").Purpose || gotID != id || recoveredID != id {
		t.Fatalf("crash did not roll back atomically: %+v %q %q %v", got, gotID, recoveredID, err)
	}
	assertAbsent(t, journalPath)
}
