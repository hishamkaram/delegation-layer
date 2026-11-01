package teams

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

func testTeam(id string) contract.Team {
	return contract.Team{
		SchemaVersion: 1, ID: id, Name: "Review team", Purpose: "Find correctness problems",
		Roles: []contract.Role{{ID: "reviewer", Name: "Reviewer", Responsibility: "Review changes"}},
		Members: []contract.Member{{
			ID: "primary", Name: "Primary", RoleIDs: []string{"reviewer"},
			ExecutionDefaults: &contract.ExecutionDefaults{Provider: "codex:exec", Model: "explicit-model"},
		}},
	}
}

func userRef(id string) task.TeamReference { return task.TeamReference{Scope: "user", TeamID: id} }

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "catalog", "teams.sqlite"), nil)
}

func registerTeam(t *testing.T, catalog *Catalog, ref task.TeamReference) (contract.Record, string) {
	t.Helper()
	record, id, err := catalog.Register(t.Context(), ref, testTeam(ref.TeamID))
	if err != nil {
		t.Fatal(err)
	}
	return record, id
}

func openTestDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", databaseURI(path, true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func TestCatalogRegistrationAndCompareAndSwap(t *testing.T) {
	catalog := testCatalog(t)
	ref := userRef("review")
	first, id := registerTeam(t, catalog, ref)
	if first.Revision != 1 || task.ValidateRootID(id) != nil {
		t.Fatalf("invalid first revision/identity: %+v %q", first, id)
	}
	if _, _, err := catalog.Register(t.Context(), ref, testTeam(ref.TeamID)); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate registration: %v", err)
	}
	definition := testTeam(ref.TeamID)
	definition.Name = "Updated team"
	updated, updateID, err := catalog.Update(t.Context(), ref, 1, definition)
	if err != nil || updated.Revision != 2 || updateID != id {
		t.Fatalf("update: %+v %q %v", updated, updateID, err)
	}
	if _, _, updateErr := catalog.Update(t.Context(), ref, 1, definition); !errors.Is(updateErr, ErrConflict) {
		t.Fatalf("stale update: %v", updateErr)
	}
	if _, deleteErr := catalog.Delete(t.Context(), ref, 1); !errors.Is(deleteErr, ErrConflict) {
		t.Fatalf("stale delete: %v", deleteErr)
	}
	got, gotID, err := New(catalog.path, nil).Lookup(t.Context(), ref)
	if err != nil || gotID != id || !reflect.DeepEqual(got, updated) {
		t.Fatalf("reopen: %+v %q %v", got, gotID, err)
	}
}

func TestCatalogTombstonePreventsABA(t *testing.T) {
	catalog := testCatalog(t)
	ref := userRef("review")
	_, id := registerTeam(t, catalog, ref)
	if revision, err := catalog.Delete(t.Context(), ref, 1); err != nil || revision != 2 {
		t.Fatalf("delete: %d %v", revision, err)
	}
	reopened := New(catalog.path, nil)
	if _, _, err := reopened.Lookup(t.Context(), ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tombstone executable: %v", err)
	}
	if _, _, err := reopened.Update(t.Context(), ref, 2, testTeam(ref.TeamID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update tombstone: %v", err)
	}
	restored, restoredID := registerTeam(t, reopened, ref)
	if restored.Revision != 3 || restoredID != id {
		t.Fatalf("re-registration reset identity/revision: %+v %q", restored, restoredID)
	}
	if _, _, err := reopened.Update(t.Context(), ref, 1, testTeam(ref.TeamID)); !errors.Is(err, ErrConflict) {
		t.Fatalf("ABA update accepted: %v", err)
	}
	if _, err := reopened.Delete(t.Context(), ref, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("ABA delete accepted: %v", err)
	}
	got, gotID, err := reopened.Lookup(t.Context(), ref)
	if err != nil || gotID != id || !reflect.DeepEqual(got, restored) {
		t.Fatalf("reopen: %+v %q %v", got, gotID, err)
	}
}

func TestResolveMemberPinsCatalogIdentity(t *testing.T) {
	first, second := testCatalog(t), testCatalog(t)
	ref := userRef("review")
	record, id := registerTeam(t, first, ref)
	_, otherID := registerTeam(t, second, ref)
	if id == otherID {
		t.Fatal("independent catalogs share identity")
	}
	target := task.TeamTarget{Team: ref, MemberID: "primary", CatalogID: id}
	resolved, err := first.ResolveMember(t.Context(), target)
	if err != nil || resolved.CatalogID != id || !reflect.DeepEqual(resolved.Record, record) || resolved.Member.ID != "primary" {
		t.Fatalf("resolve: %+v %v", resolved, err)
	}
	if _, resolveErr := second.ResolveMember(t.Context(), target); !errors.Is(resolveErr, ErrCatalogIdentity) {
		t.Fatalf("replacement catalog accepted: %v", resolveErr)
	}
	target.CatalogID = ""
	if _, resolveErr := second.ResolveMember(t.Context(), target); resolveErr != nil {
		t.Fatal(resolveErr)
	}
	target.MemberID = "missing"
	if _, resolveErr := first.ResolveMember(t.Context(), target); !errors.Is(resolveErr, ErrNotFound) {
		t.Fatalf("unknown Member: %v", resolveErr)
	}
	resolved.Record.Definition.Members[0].ExecutionDefaults.Model = "changed"
	got, _, err := first.Lookup(t.Context(), ref)
	if err != nil || got.Definition.Members[0].ExecutionDefaults.Model != "explicit-model" {
		t.Fatalf("returned value mutated catalog: %+v %v", got, err)
	}
}

func TestValidationPrecedesInitialization(t *testing.T) {
	catalog := testCatalog(t)
	ref := userRef("review")
	invalid := testTeam(ref.TeamID)
	invalid.Members[0].RoleIDs = []string{"unknown"}
	if _, _, err := catalog.Register(t.Context(), ref, invalid); !errors.Is(err, contract.ErrInvalidDefinition) {
		t.Fatalf("invalid registration: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(catalog.path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation created directory: %v", err)
	}
	wantErr := errors.New("known unsupported binding")
	catalog.validateDefaults = func(defaults contract.ExecutionDefaults) error {
		if defaults.Provider != "codex:exec" || defaults.Model != "explicit-model" {
			t.Fatalf("callback changed explicit settings: %+v", defaults)
		}
		return wantErr
	}
	if _, _, err := catalog.Register(t.Context(), ref, testTeam(ref.TeamID)); !errors.Is(err, wantErr) {
		t.Fatalf("support validation: %v", err)
	}
	if _, err := os.Stat(catalog.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("support rejection created file: %v", err)
	}
	catalog.validateDefaults = nil
	registerTeam(t, catalog, ref)
	before := readFile(t, catalog.path)
	if _, _, err := catalog.Update(t.Context(), ref, 1, invalid); !errors.Is(err, contract.ErrInvalidDefinition) {
		t.Fatalf("invalid update: %v", err)
	}
	if !reflect.DeepEqual(before, readFile(t, catalog.path)) {
		t.Fatal("invalid update changed database")
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCatalogTransactionRollbackPreservesReopen(t *testing.T) {
	catalog := testCatalog(t)
	ref := userRef("review")
	original, id := registerTeam(t, catalog, ref)
	injected := errors.New("failure after mutation")
	err := catalog.transact(t.Context(), catalogWrite, func(ctx context.Context, tx *sql.Tx, _ string) error {
		if _, err := tx.ExecContext(ctx, "UPDATE teams SET revision = 100, definition = NULL"); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("rollback failure: %v", err)
	}
	got, gotID, err := New(catalog.path, nil).Lookup(t.Context(), ref)
	if err != nil || gotID != id || !reflect.DeepEqual(got, original) {
		t.Fatalf("rollback/reopen: %+v %q %v", got, gotID, err)
	}
}
