package teams

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

func TestMissingMutationsDoNotInitializeCatalog(t *testing.T) {
	catalog := testCatalog(t)
	ref := userRef("review")
	project := task.TeamProject{Kind: "directory", Path: t.TempDir()}
	if _, _, err := catalog.Update(t.Context(), ref, 1, testTeam(ref.TeamID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing update: %v", err)
	}
	if _, err := catalog.Delete(t.Context(), ref, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing delete: %v", err)
	}
	if err := catalog.SetDefault(t.Context(), project, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing default target: %v", err)
	}
	if err := catalog.ClearDefault(t.Context(), project); err != nil {
		t.Fatalf("clear absent default: %v", err)
	}
	assertAbsent(t, filepath.Dir(catalog.path))
	if listing, err := catalog.List(t.Context(), nil); err != nil || len(listing.Records) != 0 {
		t.Fatalf("failed mutation poisoned passive read: %+v %v", listing, err)
	}
}

func TestForeignDatabaseIsNeverInitialized(t *testing.T) {
	for _, fixture := range []struct{ name, sql string }{
		{"foreign-application", "PRAGMA application_id = 123"},
		{"foreign-schema", "CREATE TABLE sqlitex (value TEXT)"},
		{"unrecognized-version", "PRAGMA user_version = 99"},
		{"incomplete-schema", "PRAGMA user_version = 1"},
		{"foreign-empty-database", "VACUUM"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			catalog := testCatalog(t)
			if err := createDatabase(t.Context(), catalog.path); err != nil {
				t.Fatal(err)
			}
			db := openTestDatabase(t, catalog.path)
			if _, err := db.ExecContext(t.Context(), fixture.sql); err != nil {
				t.Fatal(err)
			}
			before := readFile(t, catalog.path)
			if _, _, err := catalog.Register(t.Context(), userRef("review"), testTeam("review")); !errors.Is(err, ErrSchema) {
				t.Fatalf("foreign database accepted: %v", err)
			}
			if !bytes.Equal(before, readFile(t, catalog.path)) {
				t.Fatal("foreign database was changed")
			}
		})
	}
}

func TestUnsupportedJournalModeIsNotChanged(t *testing.T) {
	catalog := testCatalog(t)
	registerTeam(t, catalog, userRef("review"))
	db := openTestDatabase(t, catalog.path)
	if _, err := db.ExecContext(t.Context(), "PRAGMA journal_mode = WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.List(t.Context(), nil); !errors.Is(err, ErrSchema) {
		t.Fatalf("unsupported journal read: %v", err)
	}
	if _, _, err := catalog.Register(t.Context(), userRef("other"), testTeam("other")); !errors.Is(err, ErrSchema) {
		t.Fatalf("unsupported journal mutation: %v", err)
	}
	var mode string
	if err := db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode changed: %q %v", mode, err)
	}
}

func TestCloseFailureDoesNotHideOperationFailure(t *testing.T) {
	operationErr, closeErr := errors.New("operation failure"), errors.New("close failure")
	err := joinClose(operationErr, closeErr)
	if !errors.Is(err, operationErr) || !errors.Is(err, closeErr) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("close classification lost a cause: %v", err)
	}
}

func TestCorruptMutationDoesNotEraseData(t *testing.T) {
	catalog := testCatalog(t)
	if err := os.MkdirAll(filepath.Dir(catalog.path), 0o700); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("unrecognized private database bytes")
	if err := os.WriteFile(catalog.path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := catalog.Register(t.Context(), userRef("review"), testTeam("review")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("corrupt mutation: %v", err)
	}
	if !bytes.Equal(corrupt, readFile(t, catalog.path)) {
		t.Fatal("mutation erased corrupt data")
	}
}

func TestDefinitionSizeAndCancellationAreCheckedBeforeCreation(t *testing.T) {
	catalog := testCatalog(t)
	definition := testTeam("review")
	definition.Purpose = strings.Repeat("x", task.MaxControlRecordSize)
	if _, _, err := catalog.Register(t.Context(), userRef("review"), definition); !errors.Is(err, contract.ErrInvalidDefinition) {
		t.Fatalf("oversize definition: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := catalog.Register(ctx, userRef("review"), testTeam("review")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled mutation: %v", err)
	}
	assertAbsent(t, filepath.Dir(catalog.path))
}

func TestCatalogIdentityCannotBeChangedOrRemoved(t *testing.T) {
	catalog := testCatalog(t)
	_, id := registerTeam(t, catalog, userRef("review"))
	db := openTestDatabase(t, catalog.path)
	for _, statement := range []string{
		"UPDATE catalog_metadata SET catalog_id = '00000000000000000000000000000000'",
		"DELETE FROM catalog_metadata",
		"DELETE FROM teams",
	} {
		if _, err := db.ExecContext(t.Context(), statement); err == nil {
			t.Fatalf("identity/tombstone mutation accepted: %s", statement)
		}
	}
	if _, gotID, err := catalog.Lookup(t.Context(), userRef("review")); err != nil || gotID != id {
		t.Fatalf("identity changed: %q %v", gotID, err)
	}
}

func TestInitializationAndIdentityRollbackWithFailedFirstWrite(t *testing.T) {
	catalog := testCatalog(t)
	injected := errors.New("first write failure")
	err := catalog.transact(t.Context(), catalogCreate, func(ctx context.Context, tx *sql.Tx, _ string) error {
		if _, insertErr := tx.ExecContext(ctx, "INSERT INTO teams VALUES ('user', '', '', 'review', 1, ?)", []byte("incomplete")); insertErr != nil {
			return insertErr
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("first write failure: %v", err)
	}
	if _, _, err := catalog.Lookup(t.Context(), userRef("review")); !errors.Is(err, ErrSchema) {
		t.Fatalf("passive read accepted incomplete initialization: %v", err)
	}
	got, _ := registerTeam(t, New(catalog.path, nil), userRef("review"))
	if got.Revision != 1 {
		t.Fatalf("rolled-back initialization retained a revision: %+v", got)
	}
}
