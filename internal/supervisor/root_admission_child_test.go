package supervisor

import (
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func TestOwnedRootAdmissionWaitsForFailedLifetimeChildCleanup(t *testing.T) {
	fixture := newHelperAdmissionFixture(t)
	peer, err := taskdir.OpenStore(fixture.store.Root)
	if peer != nil {
		fixture.otherStores = append(fixture.otherStores, peer)
	}
	if err != nil {
		t.Fatal(err)
	}
	held := openHelperControl(t, fixture, peer)
	owner, err := held.AcquireLifetimeOwner(t.Context())
	fixture.owners = append(fixture.owners, owner)
	if err != nil {
		t.Fatal(err)
	}
	contender := openHelperControl(t, fixture, fixture.store)
	root, err := fixture.store.AcquireRootAdmissionContext(t.Context())
	fixture.own(root)
	if err != nil {
		t.Fatal(err)
	}
	fault := &helperMaintenanceCleanupFault{cause: errors.New("child maintenance unwind failed")}
	fault.enabled.Store(true)
	fixture.store.SetFaultInjector(fault)
	t.Cleanup(func() { fault.enabled.Store(false) })
	partial, acquisitionErr := contender.TryAcquireLifetimeOwnerContext(t.Context())
	fixture.owners = append(fixture.owners, partial)
	if partial == nil || !errors.Is(acquisitionErr, fault.cause) {
		t.Fatalf("actual child failed unwind did not retain ownership: %v", acquisitionErr)
	}
	parent := requireHelperChildAdmissionRetained(t, root, acquisitionErr, fault)
	requireHelperRootBusy(t, peer)
	fault.enabled.Store(false)
	if err = partial.Release(); err != nil {
		t.Fatal(err)
	}
	if err = parent.Lease().Release(); err != nil {
		t.Fatal(err)
	}
	exclusive, err := peer.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(exclusive)
	if err != nil {
		t.Fatalf("explicit child-first cleanup left the parent held: %v", err)
	}
}

func requireHelperChildAdmissionRetained(t *testing.T, root *taskdir.RootAdmissionLease, acquisitionErr error, fault *helperMaintenanceCleanupFault) *RootAdmissionCleanupError {
	t.Helper()
	retained := ReleaseOwnedRootAdmission(root, acquisitionErr)
	var parent *RootAdmissionCleanupError
	if !errors.As(retained, &parent) || parent.Lease() != root || !errors.Is(retained, fault.cause) {
		t.Fatalf("failed child lost its owned parent: %v", retained)
	}
	repeated := ReleaseOwnedRootAdmission(root, retained)
	var repeatedParent *RootAdmissionCleanupError
	if !errors.As(repeated, &repeatedParent) || repeatedParent != parent || reflect.TypeOf(repeated) != reflect.TypeFor[*RootAdmissionCleanupError]() || fault.attempts.Load() != 1 {
		t.Fatalf("failed unwind was retried implicitly: attempts=%d", fault.attempts.Load())
	}
	return parent
}

func openHelperControl(t *testing.T, fixture *helperAdmissionFixture, store *taskdir.Store) *taskdir.ControlDir {
	t.Helper()
	control, err := store.OpenInspectionContext(t.Context(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true)
	if control != nil {
		fixture.controls = append(fixture.controls, control)
	}
	if err != nil {
		t.Fatal(err)
	}
	return control
}

func requireHelperRootBusy(t *testing.T, store *taskdir.Store) {
	t.Helper()
	exclusive, err := store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	if exclusive != nil {
		if closeErr := exclusive.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("parent released before failed child settled: %v", err)
	}
}

type helperMaintenanceCleanupFault struct {
	taskdir.BaseFaultInjector
	enabled  atomic.Bool
	attempts atomic.Int32
	cause    error
}

func (*helperMaintenanceCleanupFault) OnLockFileBarrier(string) error      { return nil }
func (*helperMaintenanceCleanupFault) OnLockParentDirBarrier(string) error { return nil }
func (f *helperMaintenanceCleanupFault) OnFlock(path string, operation int) error {
	if f.enabled.Load() && filepath.Base(path) == ".maintenance.lock" && operation == unix.LOCK_UN {
		f.attempts.Add(1)
		return f.cause
	}
	return nil
}
