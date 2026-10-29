package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestTaskIDsAbsentParentListsNothingAndCreatesNothing(t *testing.T) {
	store := testStore(t)
	before := snapshotTree(t, store.Root)
	ids, err := store.TaskIDs(t.Context())
	if err != nil || ids == nil || len(ids) != 0 {
		t.Fatalf("absent parent = %v, %v; want empty list and nil", ids, err)
	}
	if _, err = os.Lstat(filepath.Join(store.Root, "tasks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listing created the tasks parent: %v", err)
	}
	if after := snapshotTree(t, store.Root); !reflect.DeepEqual(after, before) {
		t.Fatalf("listing changed the store:\nbefore %v\nafter  %v", before, after)
	}
	requireMaintenanceReleased(t, store)
}

func TestTaskIDsEmptyParentListsNothing(t *testing.T) {
	store := testStore(t)
	tasksDir(t, store)
	ids, err := store.TaskIDs(t.Context())
	if err != nil || ids == nil || len(ids) != 0 {
		t.Fatalf("empty parent = %v, %v; want empty list and nil", ids, err)
	}
	requireMaintenanceReleased(t, store)
}

// Prepared tasks and a bare private directory with no locks or records are
// all candidates, returned in sorted order. Every prepared record is replaced
// by unreadable garbage, so a listing that read or validated any record would
// fail; the store tree is unchanged, so it created no directory, file or lock.
func TestTaskIDsListsSortedCandidatesWithoutReadingOrCreating(t *testing.T) {
	store := testStore(t)
	var want []string
	for range 3 {
		td := preparedTask(t, store)
		must(t, filepath.WalkDir(td.Dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() || strings.HasSuffix(path, ".lock") {
				return err
			}
			if err = os.WriteFile(path, []byte("not a record"), 0o600); err != nil {
				return err
			}
			return os.Chmod(path, 0)
		}))
		want = append(want, td.TaskID)
	}
	partial := newTaskID(t)
	must(t, os.Mkdir(filepath.Join(store.Root, "tasks", partial), 0o700))
	want = append(want, partial)
	slices.Sort(want)
	before := snapshotTree(t, store.Root)

	ids, err := store.TaskIDs(t.Context())
	if err != nil || !slices.Equal(ids, want) {
		t.Fatalf("candidates = %v, %v; want %v and nil", ids, err, want)
	}
	if after := snapshotTree(t, store.Root); !reflect.DeepEqual(after, before) {
		t.Fatalf("listing changed the store:\nbefore %v\nafter  %v", before, after)
	}
	requireMaintenanceReleased(t, store)
}

func TestTaskIDsRefusesNonCanonicalEntries(t *testing.T) {
	directory := func(name string, mode os.FileMode) func(*testing.T, string, string) string {
		return func(t *testing.T, parent, _ string) string {
			path := filepath.Join(parent, name)
			must(t, os.Mkdir(path, 0o700))
			must(t, os.Chmod(path, mode))
			return name
		}
	}
	cases := []struct {
		name  string
		entry func(t *testing.T, parent, valid string) string
	}{
		{name: "malformed name", entry: directory("not-a-task", 0o700)},
		{name: "uppercase identity", entry: directory(strings.Repeat("A", 32), 0o700)},
		{name: "short identity", entry: directory(strings.Repeat("a", 31), 0o700)},
		{name: "unsafe directory mode", entry: func(t *testing.T, parent, _ string) string {
			return directory(newTaskID(t), 0o755)(t, parent, "")
		}},
		{name: "regular file", entry: func(t *testing.T, parent, _ string) string {
			id := newTaskID(t)
			must(t, os.WriteFile(filepath.Join(parent, id), nil, 0o600))
			return id
		}},
		{name: "symlink to a task", entry: func(t *testing.T, parent, valid string) string {
			id := newTaskID(t)
			must(t, os.Symlink(filepath.Join(parent, valid), filepath.Join(parent, id)))
			return id
		}},
		{name: "dangling symlink", entry: func(t *testing.T, parent, _ string) string {
			id := newTaskID(t)
			must(t, os.Symlink(filepath.Join(parent, "missing"), filepath.Join(parent, id)))
			return id
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			valid := preparedTask(t, store)
			name := test.entry(t, filepath.Join(store.Root, "tasks"), valid.TaskID)

			ids, err := store.TaskIDs(t.Context())
			if ids != nil || !errors.Is(err, task.ErrEvidenceFault) || !strings.Contains(err.Error(), "task entry "+`"`+name+`"`) {
				t.Fatalf("entry %q = %v, %v; want named task evidence fault and no list", name, ids, err)
			}
			requireMaintenanceReleased(t, store)
		})
	}
}

func TestTaskIDsRefusesUnusableParent(t *testing.T) {
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
			test.setup(t, filepath.Join(store.Root, "tasks"))
			ids, err := store.TaskIDs(t.Context())
			if ids != nil || err == nil || directoryAbsent(err) {
				t.Fatalf("unusable parent = %v, %v; want a non-absence refusal", ids, err)
			}
			requireMaintenanceReleased(t, store)
		})
	}
}

func TestTaskIDsHonorsCallerLifetime(t *testing.T) {
	store := testStore(t)
	preparedTask(t, store)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if ids, listErr := store.TaskIDs(canceled); ids != nil || !errors.Is(listErr, context.Canceled) {
		t.Fatalf("canceled listing = %v, %v; want context.Canceled", ids, listErr)
	}
	var missing context.Context
	if ids, listErr := store.TaskIDs(missing); ids != nil || !errors.Is(listErr, context.Canceled) {
		t.Fatalf("nil-context listing = %v, %v; want context.Canceled", ids, listErr)
	}

	must(t, store.maintLock.LockEX(t.Context()))
	deadline, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stop()
	ids, err := store.TaskIDs(deadline)
	must(t, store.maintLock.Unlock())
	if ids != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("listing during maintenance = %v, %v; want deadline", ids, err)
	}
	requireMaintenanceReleased(t, store)
}

func TestTaskIDsRefusesClosedStore(t *testing.T) {
	store, err := InitStore(filepath.Join(t.TempDir(), "state"))
	must(t, err)
	tasksDir(t, store)
	must(t, store.Close())
	if ids, listErr := store.TaskIDs(t.Context()); ids != nil || listErr == nil {
		t.Fatalf("closed store listing = %v, %v; want refusal", ids, listErr)
	}
}

// A maintenance release fault invalidates the listing, whether the scan
// succeeded or refused an entry, and is joined to the scan's own fault rather
// than replacing it. The injected EIO replaces the store's exact maintenance
// LOCK_UN before the kernel call, so the native shared lease is still held
// afterwards; the listing's own reference is then released by retrying Unlock
// through the same store before Store.Close.
func TestTaskIDsJoinsMaintenanceReleaseFault(t *testing.T) {
	for name, malformed := range map[string]bool{"clean scan": false, "refused entry": true} {
		t.Run(name, func(t *testing.T) {
			store, err := InitStore(filepath.Join(t.TempDir(), "state"))
			must(t, err)
			// retained marks the listing's shared reference whose release was
			// faulted; only that known reference is retried.
			retained := false
			t.Cleanup(func() {
				store.SetFaultInjector(nil)
				var releaseErr error
				if retained {
					releaseErr = store.maintLock.Unlock()
				}
				must(t, errors.Join(releaseErr, store.Close()))
			})
			parent := tasksDir(t, store)
			must(t, os.Mkdir(filepath.Join(parent, newTaskID(t)), 0o700))
			if malformed {
				must(t, os.Mkdir(filepath.Join(parent, "not-a-task"), 0o700))
			}
			fault := &lifetimeOwnerReleaseFault{failPath: ".maintenance.lock", failures: []error{unix.EIO}}
			store.SetFaultInjector(fault)
			ids, listErr := store.TaskIDs(t.Context())
			store.SetFaultInjector(nil)
			retained = len(fault.failures) == 0
			if ids != nil || !errors.Is(listErr, unix.EIO) || errors.Is(listErr, task.ErrEvidenceFault) != malformed {
				t.Fatalf("listing = %v, %v; want the joined maintenance release fault (entry fault %t)", ids, listErr, malformed)
			}
			if calls := fault.unlockCalls(".maintenance.lock"); calls != 1 || len(fault.failures) != 0 {
				t.Fatalf("maintenance unlock calls = %d with %d unused faults; want one injected release", calls, len(fault.failures))
			}

			other, err := OpenStore(store.Root)
			must(t, err)
			t.Cleanup(func() { must(t, other.Close()) })
			if err = other.maintLock.LockEXNonblocking(); !errors.Is(err, task.ErrLockBusy) {
				t.Fatalf("maintenance lease after the failed release = %v; want it still held", err)
			}
			must(t, store.maintLock.Unlock())
			retained = false
			must(t, store.Close())
			must(t, other.maintLock.LockEXNonblocking())
			must(t, other.maintLock.Unlock())
		})
	}
}
