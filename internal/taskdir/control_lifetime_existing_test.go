package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

type existingControlFixture struct {
	parent   string
	store    *Store
	controls []*ControlDir
	owners   []*LifetimeOwnerLease
}

func newExistingControlFixture(t *testing.T) *existingControlFixture {
	t.Helper()
	parent, err := os.MkdirTemp("", "existing-control-")
	must(t, err)
	fixture := &existingControlFixture{parent: parent}
	t.Cleanup(func() {
		var closeErr error
		for index := len(fixture.owners) - 1; index >= 0; index-- {
			closeErr = errors.Join(closeErr, fixture.owners[index].Release())
		}
		for index := len(fixture.controls) - 1; index >= 0; index-- {
			closeErr = errors.Join(closeErr, fixture.controls[index].Close())
		}
		if fixture.store != nil {
			closeErr = errors.Join(closeErr, fixture.store.Close())
		}
		if closeErr != nil {
			t.Errorf("retain existing-control fixture %s: %v", fixture.parent, closeErr)
			return
		}
		must(t, os.RemoveAll(fixture.parent))
	})
	fixture.store, err = InitStore(filepath.Join(parent, "state"))
	must(t, err)
	return fixture
}

func (f *existingControlFixture) control(t *testing.T) *ControlDir {
	t.Helper()
	control, err := f.store.OpenInspectionContext(t.Context(), strings.Repeat("a", 32), true)
	if control != nil {
		f.controls = append(f.controls, control)
	}
	must(t, err)
	return control
}

func (f *existingControlFixture) own(lease *LifetimeOwnerLease) {
	if lease != nil {
		f.owners = append(f.owners, lease)
	}
}

type existingControlEntry struct {
	mode os.FileMode
	data string
	link string
}

func existingControlTree(t *testing.T, root string) map[string]existingControlEntry {
	t.Helper()
	tree := map[string]existingControlEntry{}
	must(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		item := existingControlEntry{mode: info.Mode()}
		if info.Mode().IsRegular() {
			var data []byte
			data, err = os.ReadFile(path)
			item.data = string(data)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			item.link, err = os.Readlink(path)
		}
		if err != nil {
			return err
		}
		tree[path] = item
		return nil
	}))
	return tree
}

func TestControlExistingLifetimeAbsentCreatesNothing(t *testing.T) {
	fixture := newExistingControlFixture(t)
	control := fixture.control(t)
	must(t, os.WriteFile(filepath.Join(control.dir, "sentinel.json"), []byte("unchanged bytes"), 0o600))
	before := existingControlTree(t, fixture.store.Root)
	lease, present, err := control.TryAcquireExistingLifetimeOwnerContext(t.Context())
	fixture.own(lease)
	if after := existingControlTree(t, fixture.store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("existing-only observer changed tree bytes or mode")
	}
	if err != nil || present || lease != nil {
		t.Fatalf("whole absent owner: present=%t error=%v", present, err)
	}
	requireMaintenanceReleased(t, fixture.store)
}

func TestControlExistingLifetimeAcquiresExistingAndReportsBusy(t *testing.T) {
	fixture := newExistingControlFixture(t)
	first, second := fixture.control(t), fixture.control(t)
	initial, err := first.AcquireLifetimeOwner(t.Context())
	fixture.own(initial)
	must(t, err)
	before := existingControlTree(t, fixture.store.Root)
	busyCtx, cancelBusy := context.WithTimeout(t.Context(), time.Second)
	defer cancelBusy()
	busy, present, err := second.TryAcquireExistingLifetimeOwnerContext(busyCtx)
	fixture.own(busy)
	if !present || !errors.Is(err, task.ErrLockBusy) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("existing busy owner: present=%t error=%v", present, err)
	}
	must(t, initial.Release())
	lease, present, err := second.TryAcquireExistingLifetimeOwnerContext(t.Context())
	fixture.own(lease)
	if err != nil || !present || lease == nil {
		t.Fatalf("existing acquired owner: present=%t error=%v", present, err)
	}
	if after := existingControlTree(t, fixture.store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("existing acquisition changed tree bytes or mode")
	}
	if err = fixture.store.Close(); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("existing lease lost maintenance protection: %v", err)
	}
	must(t, lease.Release())
	must(t, lease.Release())
	requireMaintenanceReleased(t, fixture.store)
}

func TestControlExistingLifetimeCancellationAndClosedRefuseWithoutWrites(t *testing.T) {
	fixture := newExistingControlFixture(t)
	control := fixture.control(t)
	before := existingControlTree(t, fixture.store.Root)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	lease, present, err := control.TryAcquireExistingLifetimeOwnerContext(ctx)
	fixture.own(lease)
	if !errors.Is(err, context.Canceled) || present {
		t.Fatalf("canceled observer: %v", err)
	}
	must(t, control.Close())
	lease, present, err = control.TryAcquireExistingLifetimeOwnerContext(t.Context())
	fixture.own(lease)
	if !errors.Is(err, os.ErrClosed) || present {
		t.Fatalf("closed observer: %v", err)
	}
	if after := existingControlTree(t, fixture.store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("refused observation changed storage")
	}
}

func TestControlExistingLifetimeRefusesUnsafeInodes(t *testing.T) {
	for _, kind := range []string{"mode", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newExistingControlFixture(t)
			control := fixture.control(t)
			path := filepath.Join(control.dir, controlLifetimeOwnerLockName)
			switch kind {
			case "mode":
				must(t, os.WriteFile(path, nil, 0o644))
			case "symlink":
				must(t, os.WriteFile(path+"-target", []byte("preserve"), 0o600))
				must(t, os.Symlink(filepath.Base(path)+"-target", path))
			case "directory":
				must(t, os.Mkdir(path, 0o700))
			}
			before := existingControlTree(t, fixture.store.Root)
			lease, _, err := control.TryAcquireExistingLifetimeOwnerContext(t.Context())
			fixture.own(lease)
			if err == nil || task.RecordAbsent(err) {
				t.Fatalf("unsafe %s became free/absent: %v", kind, err)
			}
			if after := existingControlTree(t, fixture.store.Root); !reflect.DeepEqual(before, after) {
				t.Fatal("unsafe observer modified storage")
			}
			requireMaintenanceReleased(t, fixture.store)
		})
	}
}

type existingControlFault struct {
	BaseFaultInjector
	barrier error
	cancel  context.CancelFunc
	unlocks int
	fault   error
}

func (f *existingControlFault) OnLockFileBarrier(path string) error {
	if filepath.Base(path) != controlLifetimeOwnerLockName {
		return nil
	}
	if f.cancel != nil {
		f.cancel()
	}
	return f.barrier
}
func (*existingControlFault) OnLockParentDirBarrier(string) error { return nil }
func (f *existingControlFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == ".maintenance.lock" && operation == unix.LOCK_UN && f.unlocks > 0 {
		f.unlocks--
		return f.fault
	}
	return nil
}

func TestControlExistingLifetimeJoinedAbsenceFaultRetainsCleanup(t *testing.T) {
	fixture := newExistingControlFixture(t)
	control := fixture.control(t)
	fault := &existingControlFault{unlocks: 1, fault: errors.New("maintenance release fault")}
	fixture.store.SetFaultInjector(fault)
	defer func() { fault.unlocks = 0 }()
	lease, present, err := control.TryAcquireExistingLifetimeOwnerContext(t.Context())
	fixture.own(lease)
	if lease == nil || present || !errors.Is(err, os.ErrNotExist) || !errors.Is(err, fault.fault) || task.RecordAbsent(err) {
		t.Fatalf("joined absence lost cleanup/fault: present=%t error=%v", present, err)
	}
	must(t, lease.Release())
	requireMaintenanceReleased(t, fixture.store)
}

func TestControlExistingLifetimeBarrierAndMidOpenCancellationStayFaults(t *testing.T) {
	for _, kind := range []string{"joined-barrier", "cancel"} {
		t.Run(kind, func(t *testing.T) { checkExistingControlOpenFault(t, kind) })
	}
}

func checkExistingControlOpenFault(t *testing.T, kind string) {
	t.Helper()
	fixture := newExistingControlFixture(t)
	control := fixture.control(t)
	initial, err := control.AcquireLifetimeOwner(t.Context())
	fixture.own(initial)
	must(t, err)
	must(t, initial.Release())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fault := &existingControlFault{fault: errors.New("existing barrier fault")}
	if kind == "joined-barrier" {
		fault.barrier = errors.Join(os.ErrNotExist, fault.fault)
	} else {
		fault.cancel = cancel
	}
	fixture.store.SetFaultInjector(fault)
	before := existingControlTree(t, fixture.store.Root)
	lease, present, err := control.TryAcquireExistingLifetimeOwnerContext(ctx)
	fixture.own(lease)
	if err == nil || task.RecordAbsent(err) {
		t.Fatalf("observation fault became absence: %v", err)
	}
	if kind == "joined-barrier" && !errors.Is(err, fault.fault) {
		t.Fatalf("barrier cause lost: %v", err)
	}
	if kind == "cancel" && (!present || !errors.Is(err, context.Canceled)) {
		t.Fatalf("mid-open cancellation lost: %v", err)
	}
	if after := existingControlTree(t, fixture.store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("failed existing observer modified bytes/mode")
	}
	requireMaintenanceReleased(t, fixture.store)
}
