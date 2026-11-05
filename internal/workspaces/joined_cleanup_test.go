package workspaces

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

type joinedLifetimeFault struct {
	taskdir.BaseFaultInjector
	attempts int
	cause    error
}

func (*joinedLifetimeFault) OnLockFileBarrier(string) error      { return nil }
func (*joinedLifetimeFault) OnLockParentDirBarrier(string) error { return nil }
func (f *joinedLifetimeFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == ".lifetime-owner.lock" && operation == unix.LOCK_UN {
		f.attempts++
		return f.cause
	}
	return nil
}

func joinedLifetimeChild(t *testing.T, store *taskdir.Store, id string) (*taskdir.LifetimeOwnerLease, *joinedLifetimeFault, error) {
	t.Helper()
	store.SetFaultInjector(nil)
	control, err := store.OpenInspectionContext(t.Context(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := control.AcquireLifetimeOwner(t.Context())
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if closeErr := errors.Join(lease.Release(), control.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	fault := &joinedLifetimeFault{cause: errors.New("joined lifetime release fault")}
	store.SetFaultInjector(fault)
	cause := lease.ReleaseWithError(errors.New(id))
	if cause == nil || !lease.CleanupPending() {
		t.Fatal("fixture did not retain native child")
	}
	return lease, fault, cause
}

// Admission owns parent cleanup; each child helper owns its native lease and
// control. The fixture retains the exact handles used by the assertions.
type joinedAdmissionChildren struct {
	store     *taskdir.Store
	admission *Admission
	root      *taskdir.RootAdmissionLease
	first     *taskdir.LifetimeOwnerLease
	second    *taskdir.LifetimeOwnerLease
	foreign   *taskdir.LifetimeOwnerLease
	fault     *joinedLifetimeFault
	cause     error
}

func newJoinedAdmissionChildren(t *testing.T) *joinedAdmissionChildren {
	t.Helper()
	store, resource := workspacefixture.NewResource(t)
	foreignStore, err := taskdir.OpenStore(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := foreignStore.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	foreign, _, foreignErr := joinedLifetimeChild(t, foreignStore, "dddddddddddddddddddddddddddddddd")
	historicalLease, _, historical := joinedLifetimeChild(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	store.SetFaultInjector(nil)
	if err = historicalLease.Release(); err != nil {
		t.Fatal(err)
	}
	first, _, firstErr := joinedLifetimeChild(t, store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	second, fault, secondErr := joinedLifetimeChild(t, store, "cccccccccccccccccccccccccccccccc")
	store.SetFaultInjector(nil)
	root, err := store.AcquireRootAdmissionContext(t.Context())
	admission := &Admission{resource: resource, rootAdmission: root}
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if closeErr := admission.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return &joinedAdmissionChildren{
		store: store, admission: admission, root: root,
		first: first, second: second, foreign: foreign, fault: fault,
		cause: errors.Join(historical, foreignErr, firstErr, fmt.Errorf("nested: %w", secondErr), firstErr),
	}
}

func (f *joinedAdmissionChildren) requireFailedCloseRetainsParents(t *testing.T) {
	t.Helper()
	f.store.SetFaultInjector(f.fault)
	before := f.fault.attempts
	if err := f.admission.Close(); err == nil || f.fault.attempts != before+1 || !f.root.CleanupPending() || !f.second.CleanupPending() {
		t.Fatal("failed first explicit child cleanup retried or released remaining ownership")
	}
}

func TestJoinedAdmissionRetainsAllExactStoreChildren(t *testing.T) {
	fixture := newJoinedAdmissionChildren(t)
	admission, store, joined := fixture.admission, fixture.store, fixture.cause
	finished := admission.FinishContext(t.Context(), store, joined, false, nil)
	var retained *AdmissionCleanupError
	if !errors.As(finished.CloseError, &retained) || retained.Admission() != admission || !errors.Is(finished.CloseError, joined) {
		t.Fatal("historical/foreign child hid exact pending siblings or lost original cause")
	}
	fixture.requireFailedCloseRetainsParents(t)
	store.SetFaultInjector(nil)
	if err := admission.Close(); err != nil {
		t.Fatal(err)
	}
	if fixture.first.CleanupPending() || fixture.second.CleanupPending() || admission.CleanupPending() || fixture.root.CleanupPending() {
		t.Fatal("explicit admission cleanup dropped a joined pending sibling")
	}
	if !fixture.foreign.CleanupPending() {
		t.Fatal("admission released a different original Store's child")
	}
	if PendingError(finished.CloseError) {
		t.Fatal("historical typed fault became pending telemetry")
	}
}
