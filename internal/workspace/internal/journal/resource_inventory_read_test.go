package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

type inventoryReadFixture struct {
	parent     string
	store      *taskdir.Store
	resources  []*Resource
	registries []*taskdir.ControlDir
}

func newInventoryReadFixture(t *testing.T) *inventoryReadFixture {
	t.Helper()
	parent, err := os.MkdirTemp("", "inventory-read-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &inventoryReadFixture{parent: parent}
	t.Cleanup(func() {
		var closeErr error
		for index := len(fixture.registries) - 1; index >= 0; index-- {
			closeErr = errors.Join(closeErr, fixture.registries[index].Close())
		}
		for index := len(fixture.resources) - 1; index >= 0; index-- {
			closeErr = errors.Join(closeErr, fixture.resources[index].Close())
		}
		if fixture.store != nil {
			closeErr = errors.Join(closeErr, fixture.store.Close())
		}
		if closeErr != nil {
			t.Errorf("retain inventory reader fixture %s: %v", fixture.parent, closeErr)
			return
		}
		if removeErr := os.RemoveAll(fixture.parent); removeErr != nil {
			t.Error(removeErr)
		}
	})
	fixture.store, err = taskdir.InitStore(filepath.Join(parent, "state"))
	if err != nil {
		t.Fatal(err)
	}
	request := ResourceRequest{SchemaVersion: task.SchemaVersion, RootID: fixture.store.RootID, WorkspaceID: testID('a'), TaskID: testID('b'), RequestSHA256: testSHA('c'), SourceCwd: filepath.Join(parent, "source"), Container: filepath.Join(parent, "container"), BudgetNanos: int64(time.Minute)}
	resource, err := EstablishResource(t.Context(), fixture.store, request)
	if resource != nil {
		fixture.resources = append(fixture.resources, resource)
	}
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *inventoryReadFixture) load(t *testing.T) *Resource {
	t.Helper()
	resource, err := LoadResource(t.Context(), f.store, f.resources[0].Request().WorkspaceID)
	if resource != nil {
		f.resources = append(f.resources, resource)
	}
	if err != nil {
		t.Fatalf("ordinary LoadResource contract changed: %v", err)
	}
	return resource
}

func (f *inventoryReadFixture) registryProbe(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	registry, err := f.store.OpenWorkspaceRegistry(ctx, false)
	if registry != nil {
		f.registries = append(f.registries, registry)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.WithLock(ctx, func(*taskdir.ControlTransaction) error { return nil }); err != nil {
		t.Fatalf("reader retained a registry/maintenance lease: %v", err)
	}
}

type inventoryReadEntry struct {
	mode os.FileMode
	data string
	link string
}

func inventoryReadTree(t *testing.T, root string) map[string]inventoryReadEntry {
	t.Helper()
	tree := map[string]inventoryReadEntry{}
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		entry := inventoryReadEntry{mode: info.Mode()}
		var readErr error
		if info.Mode().IsRegular() {
			var data []byte
			data, readErr = os.ReadFile(path)
			entry.data = string(data)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			entry.link, readErr = os.Readlink(path)
		}
		if readErr != nil {
			return readErr
		}
		tree[path] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func requireInventoryTreeUnchanged(t *testing.T, fixture *inventoryReadFixture, before map[string]inventoryReadEntry) {
	t.Helper()
	if after := inventoryReadTree(t, fixture.store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("inventory validation changed tree bytes/modes or repaired state")
	}
}

func TestResourceValidateInventoryMatchingBindingIsReadOnly(t *testing.T) {
	fixture := newInventoryReadFixture(t)
	resource := fixture.load(t)
	before := inventoryReadTree(t, fixture.store.Root)
	if err := resource.ValidateInventory(t.Context()); err != nil {
		t.Fatal(err)
	}
	requireInventoryTreeUnchanged(t, fixture, before)
	fixture.registryProbe(t)
	if _, err := resource.control.RecordNamesContext(t.Context()); err != nil {
		t.Fatalf("reader closed borrowed resource control: %v", err)
	}
	if _, err := fixture.store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("reader closed borrowed Store: %v", err)
	}
}

func TestResourceValidateInventoryRejectsRequestTupleDrift(t *testing.T) {
	for _, kind := range []string{"task", "request-sha"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newInventoryReadFixture(t)
			changed := fixture.resources[0].Request()
			if kind == "task" {
				changed.TaskID = testID('d')
			} else {
				changed.RequestSHA256 = testSHA('e')
			}
			data, err := task.MarshalCanonical(changed)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(fixture.store.Root, "workspaces", changed.WorkspaceID, "request.json")
			if err = os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			loaded := fixture.load(t) // Valid canonical request shape alone is intentional.
			before := inventoryReadTree(t, fixture.store.Root)
			err = loaded.ValidateInventory(t.Context())
			if !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("same-ID request %s drift accepted: %v", kind, err)
			}
			requireInventoryTreeUnchanged(t, fixture, before)
			fixture.registryProbe(t)
		})
	}
}

func TestResourceValidateInventoryRefusesMissingCorruptAndSymlinkState(t *testing.T) {
	for _, kind := range []string{"missing-registry", "missing-record", "corrupt", "symlink", "wrong-task-pair"} {
		t.Run(kind, func(t *testing.T) { checkInventoryReadFault(t, kind) })
	}
}

func checkInventoryReadFault(t *testing.T, kind string) {
	t.Helper()
	fixture := newInventoryReadFixture(t)
	request := fixture.resources[0].Request()
	registry := filepath.Join(fixture.store.Root, "workspace-registry")
	record := filepath.Join(registry, workspaceInventoryName(request.WorkspaceID))
	var err error
	switch kind {
	case "missing-registry":
		err = os.Rename(registry, registry+"-retained")
	case "missing-record":
		err = os.Rename(record, record+"-retained")
	case "corrupt":
		err = os.WriteFile(record, []byte("{corrupt registry"), 0o600)
	case "symlink":
		if err = os.Rename(registry, registry+"-retained"); err == nil {
			err = os.Symlink(filepath.Base(registry)+"-retained", registry)
		}
	case "wrong-task-pair":
		changed := inventoryRecord{SchemaVersion: task.SchemaVersion, RootID: request.RootID, WorkspaceID: testID('d'), TaskID: request.TaskID, RequestSHA256: request.RequestSHA256}
		var data []byte
		data, err = task.MarshalCanonical(changed)
		if err == nil {
			err = os.WriteFile(filepath.Join(registry, taskInventoryName(request.TaskID)), data, 0o600)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	before := inventoryReadTree(t, fixture.store.Root)
	err = fixture.resources[0].ValidateInventory(t.Context())
	if err == nil || task.RecordAbsent(err) {
		t.Fatalf("broken inventory %s became success/absence: %v", kind, err)
	}
	requireInventoryTreeUnchanged(t, fixture, before)
}

func TestResourceValidateInventoryCanceledContextCreatesNothing(t *testing.T) {
	fixture := newInventoryReadFixture(t)
	before := inventoryReadTree(t, fixture.store.Root)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := fixture.resources[0].ValidateInventory(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context cause lost: %v", err)
	}
	requireInventoryTreeUnchanged(t, fixture, before)
	fixture.registryProbe(t)
}

type inventoryReadUnlockFault struct {
	taskdir.BaseFaultInjector
	target    string
	remaining int
	fault     error
}

func (*inventoryReadUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (*inventoryReadUnlockFault) OnLockParentDirBarrier(string) error { return nil }
func (f *inventoryReadUnlockFault) OnFlock(path string, operation int) error {
	if path == f.target && operation == unix.LOCK_UN && f.remaining > 0 {
		f.remaining--
		return f.fault
	}
	return nil
}

func TestResourceValidateInventoryPreservesReadAndUnlockFaults(t *testing.T) {
	fixture := newInventoryReadFixture(t)
	request := fixture.resources[0].Request()
	registry := filepath.Join(fixture.store.Root, "workspace-registry")
	if err := os.Remove(filepath.Join(registry, workspaceInventoryName(request.WorkspaceID))); err != nil {
		t.Fatal(err)
	}
	fault := &inventoryReadUnlockFault{target: filepath.Join(registry, ".operation.lock"), remaining: 1, fault: errors.New("inventory unlock fault")}
	fixture.store.SetFaultInjector(fault)
	defer func() { fault.remaining = 0 }()
	before := inventoryReadTree(t, fixture.store.Root)
	err := fixture.resources[0].ValidateInventory(t.Context())
	if !errors.Is(err, os.ErrNotExist) || !errors.Is(err, fault.fault) || !errors.Is(err, task.ErrEvidenceFault) || task.RecordAbsent(err) {
		t.Fatalf("joined inventory read/unlock causes lost: %v", err)
	}
	requireInventoryTreeUnchanged(t, fixture, before)
	// The canonical terminal Close retries the failed transaction unlock; this
	// real native probe proves its existing handle was fully released afterward.
	fixture.registryProbe(t)
}
