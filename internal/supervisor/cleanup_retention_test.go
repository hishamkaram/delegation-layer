package supervisor

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type cleanupAsProjectionError struct{ owner error }

func (cleanupAsProjectionError) Error() string { return "typed cleanup projection" }

// errors.As requires any; it validates the target before assigning its type.
func (e cleanupAsProjectionError) As(target any) bool { return errors.As(e.owner, target) }

func TestCleanupRetainsStorePreservesCustomAsOwnerProjection(t *testing.T) {
	for _, kind := range []string{"root", "lifetime"} {
		t.Run(kind, func(t *testing.T) {
			store, settle, cause := customAsCleanupOwner(t, kind)
			assertCustomAsCleanupRetention(t, store, cause, settle)
		})
	}
}

func customAsCleanupOwner(t *testing.T, kind string) (*taskdir.Store, func() error, error) {
	t.Helper()
	if kind == "lifetime" {
		fixture, lease, child := joinedLifetimeFailure(t)
		return fixture.store, lease.Release, child
	}
	store := newGenerationStore(t)
	lease, err := store.AcquireRootAdmissionContext(t.Context())
	t.Cleanup(func() {
		if closeErr := lease.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, lease.Release, RetainOwnedRootAdmission(lease, errors.New("unresolved child"))
}

func assertCustomAsCleanupRetention(t *testing.T, store *taskdir.Store, cause error, settle func() error) {
	t.Helper()
	other, err := taskdir.OpenStore(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := other.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	projected := errors.Join(errors.New("earlier independent fault"), cleanupAsProjectionError{owner: cause})
	if !CleanupRetainsStore(projected, store) || CleanupRetainsStore(projected, other) {
		t.Fatal("custom As lost owner projection or replaced actual Store identity")
	}
	if err = settle(); err != nil {
		t.Fatal(err)
	}
	if CleanupRetainsStore(projected, store) {
		t.Fatal("custom As retained a settled historical owner")
	}
}

func TestCleanupRetainedStoreTraversesVirtualViewsAndSettledSiblings(t *testing.T) {
	store := newGenerationStore(t)
	other := newGenerationStore(t)
	child := &routedStoreRetentionError{store: store, cause: errors.New("active routed child")}
	settled := &routedStoreRetentionError{cause: errors.New("settled sibling")}
	cause := errors.Join(errors.New("ordinary fault"), settled, cleanupAsProjectionError{owner: child})
	if !CleanupHasRetainedStore(cause) || !CleanupRetainsStore(cause, store) || CleanupRetainsStore(cause, other) {
		t.Fatal("virtual cleanup custody lost child or exact Store identity")
	}
	child.store = nil
	if CleanupHasRetainedStore(cause) || CleanupHasRetainedStore(nil) || CleanupHasRetainedStore(errors.New("ordinary fault")) {
		t.Fatal("settled or ordinary failure retained ancestor dependency")
	}
}

func TestRoutedCleanupFromReopenedStoreRetainsOwnedParentLease(t *testing.T) {
	store := newGenerationStore(t)
	childStore, err := taskdir.OpenStore(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := childStore.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, err := store.AcquireRootAdmissionContext(t.Context())
	t.Cleanup(func() {
		if closeErr := lease.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.CheckRootContext(t.Context(), childStore); err != nil {
		t.Fatal(err)
	}
	child := &routedStoreRetentionError{store: childStore, cause: errors.New("active routed child")}
	retained := ReleaseOwnedRootAdmission(lease, cleanupAsProjectionError{owner: child})
	var parent *RootAdmissionCleanupError
	if !errors.As(retained, &parent) || parent == nil || parent.Lease() != lease || !lease.CleanupPending() {
		t.Fatal("virtual child on reopened Store lost parent lease owner")
	}
	child.store = nil
	if err = lease.Release(); err != nil {
		t.Fatal(err)
	}
	child.store = childStore
	retained = ReleaseOwnedRootAdmission(lease, child)
	if errors.As(retained, &parent) {
		t.Fatal("released parent was reported as new cleanup ownership")
	}
	if retained = ReleaseOwnedRootAdmission(nil, child); errors.As(retained, &parent) {
		t.Fatal("nil parent was reported as cleanup ownership")
	}
}
