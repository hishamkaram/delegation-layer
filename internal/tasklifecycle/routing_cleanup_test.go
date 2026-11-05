package tasklifecycle

import (
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type routingStoreCleanupError struct {
	store *taskdir.Store
	cause error
}

func TestRoutedCleanupFromReopenedStoreKeepsAncestorAdmission(t *testing.T) {
	fixture := newOrdinaryAdmissionFixture(t)
	childStore, err := taskdir.OpenStore(fixture.store.Root)
	if childStore != nil {
		fixture.stores = append(fixture.stores, childStore)
	}
	if err != nil {
		t.Fatal(err)
	}
	lease, err := fixture.store.AcquireRootAdmissionContext(t.Context())
	fixture.own(lease)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.CheckRootContext(t.Context(), childStore); err != nil {
		t.Fatal(err)
	}
	child := &routingStoreCleanupError{store: childStore, cause: errors.New("retained routed child")}
	cause := continuationWorkError(ContinuationResult{Failure: errors.New("earlier continuation fault"), Dispatch: &DispatchResult{Failure: child}})
	retained, err := releaseRootAdmission(lease, cause)
	if err != nil {
		t.Fatal(err)
	}
	exclusive, busyErr := childStore.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(exclusive)
	if !errors.Is(busyErr, task.ErrLockBusy) || retained != lease {
		t.Fatalf("ancestor SH released while routed child retained another Store: %v", busyErr)
	}
	child.store = nil
	if retained, err = releaseRootAdmission(lease, cause); err != nil || retained != nil {
		t.Fatal("settled child prevented ancestor admission release")
	}
	exclusive, err = childStore.TryAcquireRootAdmissionExclusiveContext(t.Context())
	fixture.own(exclusive)
	if err != nil {
		t.Fatal(err)
	}
}

func (e *routingStoreCleanupError) Error() string                 { return e.cause.Error() }
func (e *routingStoreCleanupError) Unwrap() error                 { return e.cause }
func (e *routingStoreCleanupError) RetainedStore() *taskdir.Store { return e.store }

type routedCleanupProjectionError struct{ owner error }

func (routedCleanupProjectionError) Error() string { return "projected routed cleanup" }

// errors.As requires any and validates the target before delegation.
func (e routedCleanupProjectionError) As(target any) bool { return errors.As(e.owner, target) }

func TestAdmissionCleanupFaultPreservesVirtualHistoricalOwner(t *testing.T) {
	child := &routingStoreCleanupError{cause: errors.New("settled routed cleanup")}
	if !admissionCleanupFault(routedCleanupProjectionError{owner: child}) {
		t.Fatal("virtual view erased concrete historical cleanup failure")
	}
}

func TestRoutedCleanupRetainsOuterAdmissionAndActualStore(t *testing.T) {
	fixture := newOrdinaryAdmissionFixture(t)
	lease, err := fixture.store.AcquireRootAdmissionContext(t.Context())
	fixture.own(lease)
	if err != nil {
		t.Fatal(err)
	}
	child := &routingStoreCleanupError{store: fixture.store, cause: errors.New("startup attempt cleanup unresolved")}
	cause := errors.Join(errors.New("earlier observation fault"), child)
	retained, err := releaseRootAdmission(lease, cause)
	if err != nil || retained != lease || !lease.CleanupPending() {
		t.Fatal("routed cleanup lost outer admission")
	}
	store, err := closeAdmissionStore(fixture.store, nil, cause)
	if err != nil || store != fixture.store {
		t.Fatal("routed cleanup lost actual Store")
	}
	if _, err = store.PinRootContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	child.store = nil
	if pendingAdmissionCleanup(cause) || !admissionCleanupFault(cause) {
		t.Fatal("settled child retained custody or erased concrete cleanup fault")
	}
	if retained, err = releaseRootAdmission(lease, cause); err != nil || retained != nil {
		t.Fatal("settled child prevented outer release")
	}
	if store, err = closeAdmissionStore(fixture.store, nil, cause); err != nil || store != nil {
		t.Fatal("settled child prevented actual Store close")
	}
	if _, err = fixture.store.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatal("settled cleanup left Store open")
	}
}
