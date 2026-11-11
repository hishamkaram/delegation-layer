package taskdir

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type cleanupAsProjectionError struct{ owner error }

func (cleanupAsProjectionError) Error() string { return "typed cleanup projection" }

// errors.As requires any; it validates the target before assigning its type.
func (e cleanupAsProjectionError) As(target any) bool { return errors.As(e.owner, target) }

func TestCleanupRetainsStorePreservesCustomAsOwnerProjection(t *testing.T) {
	fixture, acquire, held := partialLifetimeSubject(t, "control")
	must(t, held.Release())
	lease, err := acquire(t.Context(), true)
	must(t, err)
	fault := &partialOwnerFault{maintenance: true, cause: task.ErrLockBusy}
	fixture.store.SetFaultInjector(fault)
	t.Cleanup(func() {
		fault.maintenance = false
		if closeErr := lease.Release(); closeErr != nil {
			fixture.failed = true
			t.Error(closeErr)
		}
	})
	cause := lease.ReleaseWithError(errors.New("work failed"))
	if cause == nil || !lease.CleanupPending() {
		t.Fatal("fixture did not retain native cleanup")
	}
	other := fixture.other(t)
	projected := errors.Join(errors.New("earlier independent fault"), cleanupAsProjectionError{owner: cause})
	if !CleanupHasRetainedStore(projected) || !CleanupFault(projected) {
		t.Fatal("active lifetime cleanup disappeared from dependency classification")
	}
	if !CleanupRetainsStore(projected, fixture.store) || CleanupRetainsStore(projected, other) || CleanupRetainsStore(projected, nil) {
		t.Fatal("custom As lost owner projection or replaced actual Store identity")
	}
	fault.maintenance = false
	must(t, lease.Release())
	if CleanupRetainsStore(projected, fixture.store) || CleanupHasRetainedStore(projected) {
		t.Fatal("custom As retained a settled historical owner")
	}
	if !CleanupFault(projected) {
		t.Fatal("settled historical cleanup lost failure classification")
	}
}

type retainedStoreError struct {
	store *Store
	cause error
}

func (e *retainedStoreError) Error() string         { return e.cause.Error() }
func (e *retainedStoreError) Unwrap() error         { return e.cause }
func (e *retainedStoreError) RetainedStore() *Store { return e.store }

func TestCleanupRetainedStoreTraversesVirtualViewsAndSettledSiblings(t *testing.T) {
	fixture := newStoreOwnerFixture(t)
	other := fixture.other(t)
	child := &retainedStoreError{store: fixture.store, cause: errors.New("active routed child")}
	settled := &retainedStoreError{cause: errors.New("settled sibling")}
	cause := errors.Join(errors.New("ordinary fault"), settled, cleanupAsProjectionError{owner: child})
	if !CleanupHasRetainedStore(cause) || !CleanupRetainsStore(cause, fixture.store) || CleanupRetainsStore(cause, other) {
		t.Fatal("virtual cleanup custody lost child or exact Store identity")
	}
	// Another Store for the same root still retains the ancestor dependency,
	// without retaining the original Store handle.
	child.store = other
	if !CleanupHasRetainedStore(cause) || CleanupRetainsStore(cause, fixture.store) || !CleanupRetainsStore(cause, other) {
		t.Fatal("dependent cleanup was confused with exact Store custody")
	}
	child.store = nil
	if CleanupHasRetainedStore(cause) || CleanupHasRetainedStore(nil) || CleanupHasRetainedStore(errors.New("ordinary fault")) {
		t.Fatal("settled or ordinary failure retained ancestor dependency")
	}
}
