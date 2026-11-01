package teams

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path should be absent: %s: %v", path, err)
	}
}

func TestPassiveMissingReadsNeverInitialize(t *testing.T) {
	catalog := testCatalog(t)
	project := task.TeamProject{Kind: "directory", Path: t.TempDir()}
	assertAbsent(t, filepath.Dir(catalog.path))
	listing, err := catalog.List(t.Context(), &project)
	if err != nil || listing.CatalogID != "" || len(listing.Records) != 0 {
		t.Fatalf("missing list: %+v %v", listing, err)
	}
	if _, _, err := catalog.Lookup(t.Context(), userRef("review")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lookup: %v", err)
	}
	target := task.TeamTarget{Team: userRef("review"), MemberID: "primary"}
	if _, err := catalog.ResolveMember(t.Context(), target); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing resolution: %v", err)
	}
	if result, err := catalog.Default(t.Context(), project); err != nil || result.Status != "unset" {
		t.Fatalf("missing default: %+v %v", result, err)
	}
	assertAbsent(t, filepath.Dir(catalog.path))
}

func TestReadOnlyEscapedCatalogPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "space ? mode=rw # % ü", "teams ?#.sqlite")
	catalog := New(path, nil)
	ref := userRef("review")
	_, id := registerTeam(t, catalog, ref)
	before := readFile(t, path)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
			t.Error(err)
		}
	})
	catalog.validateDefaults = func(_ contract.ExecutionDefaults) error {
		t.Fatal("passive read invoked support validation")
		return nil
	}
	if got, gotID, err := catalog.Lookup(t.Context(), ref); err != nil || gotID != id || got.Revision != 1 {
		t.Fatalf("read-only lookup: %+v %q %v", got, gotID, err)
	}
	if listing, err := catalog.List(t.Context(), nil); err != nil || len(listing.Records) != 1 {
		t.Fatalf("read-only list: %+v %v", listing, err)
	}
	if !bytes.Equal(before, readFile(t, path)) {
		t.Fatal("read changed database")
	}
	assertAbsent(t, path+"-journal")
	catalog.validateDefaults = nil
	if _, _, err := catalog.Register(t.Context(), userRef("other"), testTeam("other")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("read-only mutation: %v", err)
	}
}

func TestCorruptAndUnsupportedCatalogsFailWithoutRepair(t *testing.T) {
	for _, contents := range [][]byte{[]byte("not a SQLite database"), {}} {
		catalog := testCatalog(t)
		if err := os.MkdirAll(filepath.Dir(catalog.path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(catalog.path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.List(t.Context(), nil); err == nil {
			t.Fatalf("present invalid file treated as empty: %q", contents)
		}
		if !bytes.Equal(contents, readFile(t, catalog.path)) {
			t.Fatal("passive read repaired database")
		}
	}
	catalog := testCatalog(t)
	registerTeam(t, catalog, userRef("review"))
	db := openTestDatabase(t, catalog.path)
	if _, err := db.ExecContext(t.Context(), "PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, catalog.path)
	if _, err := catalog.List(t.Context(), nil); !errors.Is(err, ErrSchema) {
		t.Fatalf("newer schema read: %v", err)
	}
	if _, _, err := catalog.Register(t.Context(), userRef("other"), testTeam("other")); !errors.Is(err, ErrSchema) {
		t.Fatalf("newer schema mutation: %v", err)
	}
	if !bytes.Equal(before, readFile(t, catalog.path)) {
		t.Fatal("unsupported schema was overwritten")
	}
}

func TestInvalidRecordDiagnosticPreservesUsableTeams(t *testing.T) {
	catalog := testCatalog(t)
	registerTeam(t, catalog, userRef("bad"))
	registerTeam(t, catalog, userRef("good"))
	db := openTestDatabase(t, catalog.path)
	if _, err := db.ExecContext(t.Context(), "UPDATE teams SET definition = ? WHERE team_id = 'bad'", []byte(`{"schema_version":99}`)); err != nil {
		t.Fatal(err)
	}
	listing, err := catalog.List(t.Context(), nil)
	if err != nil || len(listing.Records) != 1 || listing.Records[0].Reference.TeamID != "good" || len(listing.Diagnostics) != 1 {
		t.Fatalf("invalid record handling: %+v %v", listing, err)
	}
	if listing.Diagnostics[0].Code != "invalid_record" || listing.Diagnostics[0].Reference.TeamID != "bad" {
		t.Fatalf("diagnostic: %+v", listing.Diagnostics)
	}
	if _, _, err := catalog.Lookup(t.Context(), userRef("bad")); err == nil {
		t.Fatal("invalid explicit target accepted")
	}
}
