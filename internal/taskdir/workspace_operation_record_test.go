package taskdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const testOperationRecordName = "request.json"

// stateTreeSnapshot records every entry's type, mode, size and modification
// time so a read-only observation can prove it created or replaced nothing.
func stateTreeSnapshot(t *testing.T, root string) []string {
	t.Helper()
	var entries []string
	must(t, filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, fmt.Sprintf("%s %s %d %d", rel, info.Mode(), info.Size(), info.ModTime().UnixNano()))
		return nil
	}))
	return entries
}

func bareWorkspaceOperation(t *testing.T, store *Store) string {
	t.Helper()
	id := newWorkspaceOperationID(t)
	must(t, os.MkdirAll(workspaceOperationsParent(store), 0o700))
	must(t, os.Mkdir(filepath.Join(workspaceOperationsParent(store), id), 0o700))
	return id
}

func operationRecordPath(store *Store, operationID string) string {
	return filepath.Join(workspaceOperationsParent(store), operationID, testOperationRecordName)
}

func requireOperationRecordRefused(t *testing.T, store *Store, operationID string) error {
	t.Helper()
	data, present, err := store.ReadWorkspaceOperationRecord(t.Context(), operationID, testOperationRecordName)
	if data != nil || present || err == nil {
		t.Fatalf("record observation = %q, %v, %v; want a refusal without data", data, present, err)
	}
	requireMaintenanceReleased(t, store)
	return err
}

// A crash between operation mkdir and lock/request creation leaves a bare
// private directory. It is clean absence and the observation writes nothing,
// including the operation lock that LoadOperation would require.
func TestReadWorkspaceOperationRecordReportsBareOrphanAbsenceWithoutWriting(t *testing.T) {
	store := testStore(t)
	id := bareWorkspaceOperation(t, store)
	before := stateTreeSnapshot(t, store.Root)

	data, present, err := store.ReadWorkspaceOperationRecord(t.Context(), id, testOperationRecordName)
	if data != nil || present || err != nil {
		t.Fatalf("bare orphan = %q, %v, %v; want clean absence", data, present, err)
	}
	if after := stateTreeSnapshot(t, store.Root); !slices.Equal(before, after) {
		t.Fatalf("observation changed the state root:\nbefore %v\nafter  %v", before, after)
	}
	if _, err = os.Lstat(filepath.Join(workspaceOperationsParent(store), id, ".operation.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("observation created the operation lock: %v", err)
	}
	requireMaintenanceReleased(t, store)
}

// Bytes committed through the production control-record writer are returned
// exactly; an empty file and a record at the bound are both present.
func TestReadWorkspaceOperationRecordReturnsPresentBytes(t *testing.T) {
	store := testStore(t)
	id := newWorkspaceOperationID(t)
	operation, err := store.OpenWorkspaceOperation(t.Context(), id, true)
	must(t, err)
	_, err = operation.Put(testOperationRecordName, map[string]string{"operation": id})
	must(t, err)
	must(t, operation.Close())
	want, err := os.ReadFile(operationRecordPath(store, id))
	must(t, err)

	cases := map[string][]byte{"committed record": want, "empty file": {}, "record at bound": bytes.Repeat([]byte("x"), task.MaxControlRecordSize)}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			must(t, os.WriteFile(operationRecordPath(store, id), content, 0o600))
			data, present, readErr := store.ReadWorkspaceOperationRecord(t.Context(), id, testOperationRecordName)
			if readErr != nil || !present || data == nil || !bytes.Equal(data, content) {
				t.Fatalf("present record = %d bytes, %v, %v; want %d bytes", len(data), present, readErr, len(content))
			}
			requireMaintenanceReleased(t, store)
		})
	}
}

// Absence is admitted only for the leaf. A missing parent or operation
// directory is an error, and refusing it creates nothing.
func TestReadWorkspaceOperationRecordRefusesMissingOperation(t *testing.T) {
	store := testStore(t)
	id := newWorkspaceOperationID(t)
	if err := requireOperationRecordRefused(t, store, id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing parent = %v; want the parent's absence as an error", err)
	}
	if _, err := os.Lstat(workspaceOperationsParent(store)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("observation created the operations parent: %v", err)
	}
	must(t, os.Mkdir(workspaceOperationsParent(store), 0o700))
	if err := requireOperationRecordRefused(t, store, id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing operation = %v; want the directory's absence as an error", err)
	}
	if _, err := os.Lstat(filepath.Join(workspaceOperationsParent(store), id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("observation created the operation directory: %v", err)
	}
}

func TestReadWorkspaceOperationRecordRefusesUnsafeOperationDirectory(t *testing.T) {
	cases := map[string]func(t *testing.T, operation string){
		"regular file": func(t *testing.T, operation string) {
			must(t, os.WriteFile(operation, nil, 0o600))
		},
		"symlink to a private directory": func(t *testing.T, operation string) {
			target := filepath.Join(filepath.Dir(operation), "elsewhere")
			must(t, os.Mkdir(target, 0o700))
			must(t, os.Symlink(target, operation))
		},
		"unsafe mode": func(t *testing.T, operation string) {
			must(t, os.Mkdir(operation, 0o700))
			must(t, os.Chmod(operation, 0o755))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			id := newWorkspaceOperationID(t)
			must(t, os.Mkdir(workspaceOperationsParent(store), 0o700))
			setup(t, filepath.Join(workspaceOperationsParent(store), id))
			if err := requireOperationRecordRefused(t, store, id); errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe operation directory = %v; want a fault, not absence", err)
			}
		})
	}
}

func TestReadWorkspaceOperationRecordRefusesUnsafeRecords(t *testing.T) {
	cases := map[string]func(t *testing.T, record string){
		"oversized": func(t *testing.T, record string) {
			must(t, os.WriteFile(record, bytes.Repeat([]byte("x"), task.MaxControlRecordSize+1), 0o600))
		},
		"unsafe mode": func(t *testing.T, record string) {
			must(t, os.WriteFile(record, []byte("{}"), 0o600))
			must(t, os.Chmod(record, 0o644))
		},
		"unreadable": func(t *testing.T, record string) {
			if os.Geteuid() == 0 {
				t.Skip("root bypasses file read permission")
			}
			must(t, os.WriteFile(record, []byte("{}"), 0o600))
			must(t, os.Chmod(record, 0o000))
		},
		"symlink to a private record": func(t *testing.T, record string) {
			target := filepath.Join(filepath.Dir(record), "target.json")
			must(t, os.WriteFile(target, []byte("{}"), 0o600))
			must(t, os.Symlink(target, record))
		},
		"dangling symlink": func(t *testing.T, record string) {
			must(t, os.Symlink(filepath.Join(filepath.Dir(record), "missing.json"), record))
		},
		"directory": func(t *testing.T, record string) {
			must(t, os.Mkdir(record, 0o700))
		},
		"fifo": func(t *testing.T, record string) {
			must(t, makeFIFO(record))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			id := bareWorkspaceOperation(t, store)
			setup(t, operationRecordPath(store, id))
			err := requireOperationRecordRefused(t, store, id)
			if errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe record = %v; want a fault, not absence", err)
			}
			if name == "oversized" && !errors.Is(err, task.ErrControlRecordTooBig) {
				t.Fatalf("oversized record = %v; want ErrControlRecordTooBig", err)
			}
		})
	}
}

// Identities and names are validated before any lease or filesystem access.
func TestReadWorkspaceOperationRecordValidatesInputs(t *testing.T) {
	store := testStore(t)
	id := bareWorkspaceOperation(t, store)
	for _, operationID := range []string{"", "..", "../" + id, strings.ToUpper(id)} {
		if data, present, err := store.ReadWorkspaceOperationRecord(t.Context(), operationID, testOperationRecordName); data != nil || present || !errors.Is(err, task.ErrInvalidTaskID) {
			t.Fatalf("operation %q = %q, %v, %v; want refusal", operationID, data, present, err)
		}
	}
	for _, name := range []string{"", "request", ".json", "../request.json", "Request.json", ".operation.lock", "nested/request.json"} {
		data, present, err := store.ReadWorkspaceOperationRecord(t.Context(), id, name)
		if data != nil || present || !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("name %q = %q, %v, %v; want invalid-name refusal", name, data, present, err)
		}
	}
	requireMaintenanceReleased(t, store)
}

func TestReadWorkspaceOperationRecordHonorsCallerLifetime(t *testing.T) {
	store := testStore(t)
	id := bareWorkspaceOperation(t, store)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if data, present, err := store.ReadWorkspaceOperationRecord(canceled, id, testOperationRecordName); data != nil || present || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled observation = %q, %v, %v; want context.Canceled", data, present, err)
	}
	var missing context.Context
	if data, present, err := store.ReadWorkspaceOperationRecord(missing, id, testOperationRecordName); data != nil || present || !errors.Is(err, context.Canceled) {
		t.Fatalf("nil-context observation = %q, %v, %v; want context.Canceled", data, present, err)
	}

	must(t, store.maintLock.LockEX(t.Context()))
	deadline, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stop()
	data, present, err := store.ReadWorkspaceOperationRecord(deadline, id, testOperationRecordName)
	must(t, store.maintLock.Unlock())
	if data != nil || present || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("observation during maintenance = %q, %v, %v; want deadline", data, present, err)
	}
	requireMaintenanceReleased(t, store)
}

func TestReadWorkspaceOperationRecordRefusesClosedStore(t *testing.T) {
	store, err := InitStore(filepath.Join(t.TempDir(), "state"))
	must(t, err)
	id := bareWorkspaceOperation(t, store)
	must(t, store.Close())
	if data, present, readErr := store.ReadWorkspaceOperationRecord(t.Context(), id, testOperationRecordName); data != nil || present || readErr == nil {
		t.Fatalf("closed store observation = %q, %v, %v; want refusal", data, present, readErr)
	}
}

// Every owned descriptor close and the caller lifetime are joined after the
// leaf stage, so neither can be reported as clean absence or present data.
func TestReadWorkspaceOperationRecordJoinsCloseAndLifetimeFaults(t *testing.T) {
	store := testStore(t)
	absent := bareWorkspaceOperation(t, store)
	present := bareWorkspaceOperation(t, store)
	must(t, os.WriteFile(operationRecordPath(store, present), []byte("{}"), 0o600))

	faultOn := func(target string, closed *[]string) fileCloser {
		return func(f *os.File) error {
			*closed = append(*closed, f.Name())
			return errors.Join(f.Close(), func() error {
				if f.Name() == target {
					return errTestCloseFault
				}
				return nil
			}())
		}
	}
	cases := []struct {
		name, operationID, target string
	}{
		{name: "absent leaf with directory close fault", operationID: absent, target: filepath.Join(workspaceOperationsParent(store), absent)},
		{name: "present leaf with record close fault", operationID: present, target: operationRecordPath(store, present)},
		{name: "present leaf with directory close fault", operationID: present, target: filepath.Join(workspaceOperationsParent(store), present)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var closed []string
			data, found, err := store.readWorkspaceOperationRecord(t.Context(), test.operationID, testOperationRecordName, faultOn(test.target, &closed))
			if data != nil || found || !errors.Is(err, errTestCloseFault) {
				t.Fatalf("observation = %q, %v, %v; want the joined close fault", data, found, err)
			}
			if !slices.Contains(closed, test.target) || !slices.Contains(closed, filepath.Join(workspaceOperationsParent(store), test.operationID)) {
				t.Fatalf("closed descriptors = %v; want the faulted and pinned descriptors released", closed)
			}
			requireMaintenanceReleased(t, store)
		})
	}

	t.Run("cancellation after an absent leaf", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		cancelOnClose := func(f *os.File) error {
			cancel()
			return f.Close()
		}
		data, found, err := store.readWorkspaceOperationRecord(ctx, absent, testOperationRecordName, cancelOnClose)
		if data != nil || found || !errors.Is(err, context.Canceled) {
			t.Fatalf("observation = %q, %v, %v; want context.Canceled", data, found, err)
		}
		requireMaintenanceReleased(t, store)
	})
}

// A maintenance release fault after the observation invalidates it, whether
// the leaf was absent or present. The injected EIO replaces the store's exact
// maintenance LOCK_UN before the kernel call, so the native shared lease is
// still held afterwards. The observation's own reference is then released by
// retrying Unlock through the same store before Store.Close.
func TestReadWorkspaceOperationRecordMaintenanceReleaseFaultIsNotAbsence(t *testing.T) {
	for name, withRecord := range map[string]bool{"absent leaf": false, "present leaf": true} {
		t.Run(name, func(t *testing.T) {
			store, err := InitStore(filepath.Join(t.TempDir(), "state"))
			must(t, err)
			// retained marks the observation's shared reference whose release
			// was faulted; only that known reference is retried.
			retained := false
			t.Cleanup(func() {
				store.SetFaultInjector(nil)
				var releaseErr error
				if retained {
					releaseErr = store.maintLock.Unlock()
				}
				must(t, errors.Join(releaseErr, store.Close()))
			})
			id := bareWorkspaceOperation(t, store)
			if withRecord {
				must(t, os.WriteFile(operationRecordPath(store, id), []byte("{}"), 0o600))
			}
			fault := &lifetimeOwnerReleaseFault{failPath: ".maintenance.lock", failures: []error{unix.EIO}}
			store.SetFaultInjector(fault)
			data, present, readErr := store.ReadWorkspaceOperationRecord(t.Context(), id, testOperationRecordName)
			store.SetFaultInjector(nil)
			retained = len(fault.failures) == 0
			if data != nil || present || !errors.Is(readErr, unix.EIO) {
				t.Fatalf("observation = %q, %v, %v; want the joined maintenance release fault", data, present, readErr)
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
