package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

func newWorkspaceOperationID(t *testing.T) string {
	t.Helper()
	id, err := randomid.NewTaskID()
	must(t, err)
	return id
}

func workspaceOperationsParent(store *Store) string {
	return filepath.Join(store.Root, workspaceOperationsDirectory)
}

// requireMaintenanceReleased proves the listing left no shared maintenance
// lease behind, whatever its result.
func requireMaintenanceReleased(t *testing.T, store *Store) {
	t.Helper()
	must(t, store.maintLock.LockEXNonblocking())
	must(t, store.maintLock.Unlock())
}

func TestWorkspaceOperationIDsAbsentParentListsNothingAndCreatesNothing(t *testing.T) {
	store := testStore(t)
	ids, err := store.WorkspaceOperationIDs(t.Context())
	if err != nil || ids == nil || len(ids) != 0 {
		t.Fatalf("absent parent = %v, %v; want empty list and nil", ids, err)
	}
	if _, err = os.Lstat(workspaceOperationsParent(store)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listing created the operations parent: %v", err)
	}
	requireMaintenanceReleased(t, store)
}

func TestWorkspaceOperationIDsEmptyParentListsNothing(t *testing.T) {
	store := testStore(t)
	must(t, os.Mkdir(workspaceOperationsParent(store), 0o700))
	ids, err := store.WorkspaceOperationIDs(t.Context())
	if err != nil || ids == nil || len(ids) != 0 {
		t.Fatalf("empty parent = %v, %v; want empty list and nil", ids, err)
	}
}

// Journals opened through the production port and a bare orphan directory
// with no request record are all candidates, returned in sorted order.
func TestWorkspaceOperationIDsListsSortedCandidatesIncludingOrphans(t *testing.T) {
	store := testStore(t)
	var want []string
	for range 3 {
		id := newWorkspaceOperationID(t)
		operation, err := store.OpenWorkspaceOperation(t.Context(), id, true)
		must(t, err)
		must(t, operation.Close())
		want = append(want, id)
	}
	orphan := newWorkspaceOperationID(t)
	must(t, os.Mkdir(filepath.Join(workspaceOperationsParent(store), orphan), 0o700))
	want = append(want, orphan)
	slices.Sort(want)

	ids, err := store.WorkspaceOperationIDs(t.Context())
	if err != nil || !slices.Equal(ids, want) {
		t.Fatalf("candidates = %v, %v; want %v and nil", ids, err, want)
	}
	if _, err = os.Lstat(filepath.Join(workspaceOperationsParent(store), orphan, "request.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan request state changed: %v", err)
	}
	requireMaintenanceReleased(t, store)
}

type workspaceOperationEntryCase struct {
	name  string
	entry func(t *testing.T, parent, valid string) string
}

func TestWorkspaceOperationIDsRefusesNonCanonicalEntries(t *testing.T) {
	directory := func(name string, mode os.FileMode) func(*testing.T, string, string) string {
		return func(t *testing.T, parent, _ string) string {
			path := filepath.Join(parent, name)
			must(t, os.Mkdir(path, 0o700))
			must(t, os.Chmod(path, mode))
			return name
		}
	}
	cases := []workspaceOperationEntryCase{
		{name: "malformed name", entry: directory("not-an-operation", 0o700)},
		{name: "uppercase identity", entry: directory(strings.Repeat("A", 32), 0o700)},
		{name: "short identity", entry: directory(strings.Repeat("a", 31), 0o700)},
		{name: "unsafe directory mode", entry: func(t *testing.T, parent, _ string) string {
			return directory(newWorkspaceOperationID(t), 0o755)(t, parent, "")
		}},
		{name: "regular file", entry: func(t *testing.T, parent, _ string) string {
			id := newWorkspaceOperationID(t)
			must(t, os.WriteFile(filepath.Join(parent, id), nil, 0o600))
			return id
		}},
		{name: "symlink to an operation", entry: func(t *testing.T, parent, valid string) string {
			id := newWorkspaceOperationID(t)
			must(t, os.Symlink(filepath.Join(parent, valid), filepath.Join(parent, id)))
			return id
		}},
		{name: "dangling symlink", entry: func(t *testing.T, parent, _ string) string {
			id := newWorkspaceOperationID(t)
			must(t, os.Symlink(filepath.Join(parent, "missing"), filepath.Join(parent, id)))
			return id
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			valid := newWorkspaceOperationID(t)
			operation, err := store.OpenWorkspaceOperation(t.Context(), valid, true)
			must(t, err)
			must(t, operation.Close())
			name := test.entry(t, workspaceOperationsParent(store), valid)

			ids, err := store.WorkspaceOperationIDs(t.Context())
			if ids != nil || !errors.Is(err, task.ErrEvidenceFault) || !strings.Contains(err.Error(), name) {
				t.Fatalf("entry %q = %v, %v; want named evidence fault and no list", name, ids, err)
			}
			requireMaintenanceReleased(t, store)
		})
	}
}

func TestWorkspaceOperationIDsRefusesUnusableParent(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, parent string)
	}{
		{name: "regular file", setup: func(t *testing.T, parent string) {
			must(t, os.WriteFile(parent, nil, 0o600))
		}},
		{name: "dangling symlink", setup: func(t *testing.T, parent string) {
			must(t, os.Symlink(filepath.Join(filepath.Dir(parent), "missing"), parent))
		}},
		{name: "symlink to a directory", setup: func(t *testing.T, parent string) {
			target := filepath.Join(filepath.Dir(parent), "elsewhere")
			must(t, os.Mkdir(target, 0o700))
			must(t, os.Symlink(target, parent))
		}},
		{name: "unsafe mode", setup: func(t *testing.T, parent string) {
			must(t, os.Mkdir(parent, 0o700))
			must(t, os.Chmod(parent, 0o755))
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			test.setup(t, workspaceOperationsParent(store))
			ids, err := store.WorkspaceOperationIDs(t.Context())
			if ids != nil || err == nil || workspaceOperationsAbsent(err) {
				t.Fatalf("unusable parent = %v, %v; want a non-absence refusal", ids, err)
			}
			requireMaintenanceReleased(t, store)
		})
	}
}

func TestWorkspaceOperationsAbsenceRequiresEveryCauseAbsent(t *testing.T) {
	if !workspaceOperationsAbsent(errors.Join(os.ErrNotExist, os.ErrNotExist)) {
		t.Fatal("joined absence was not absence")
	}
	for _, err := range []error{nil, errTestReadFault, errors.Join(os.ErrNotExist, errTestReadFault)} {
		if workspaceOperationsAbsent(err) {
			t.Fatalf("%v classified as absence", err)
		}
	}
}

func TestWorkspaceOperationIDsHonorsCallerLifetime(t *testing.T) {
	store := testStore(t)
	operation, err := store.OpenWorkspaceOperation(t.Context(), newWorkspaceOperationID(t), true)
	must(t, err)
	must(t, operation.Close())

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if ids, listErr := store.WorkspaceOperationIDs(canceled); ids != nil || !errors.Is(listErr, context.Canceled) {
		t.Fatalf("canceled listing = %v, %v; want context.Canceled", ids, listErr)
	}
	var missing context.Context
	if ids, listErr := store.WorkspaceOperationIDs(missing); ids != nil || !errors.Is(listErr, context.Canceled) {
		t.Fatalf("nil-context listing = %v, %v; want context.Canceled", ids, listErr)
	}

	must(t, store.maintLock.LockEX(t.Context()))
	deadline, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stop()
	ids, err := store.WorkspaceOperationIDs(deadline)
	must(t, store.maintLock.Unlock())
	if ids != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("listing during maintenance = %v, %v; want deadline", ids, err)
	}
	requireMaintenanceReleased(t, store)
}

func TestWorkspaceOperationIDsRefusesClosedStore(t *testing.T) {
	store, err := InitStore(filepath.Join(t.TempDir(), "state"))
	must(t, err)
	must(t, store.Close())
	if ids, listErr := store.WorkspaceOperationIDs(t.Context()); ids != nil || listErr == nil {
		t.Fatalf("closed store listing = %v, %v; want refusal", ids, listErr)
	}
}
