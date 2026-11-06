package tasklifecycle

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestStoreWaitsForLifetimeCleanup(t *testing.T) {
	fixture := newOrdinaryAdmissionFixture(t)
	control, err := fixture.store.OpenInfrastructureContext(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := control.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fault := &admissionReleaseFault{name: ".lifetime-owner.lock", cause: task.ErrLockBusy}
	fault.enabled.Store(true)
	fixture.store.SetFaultInjector(fault)
	t.Cleanup(func() {
		fault.enabled.Store(false)
		if closeErr := errors.Join(owner.Release(), control.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	cause := owner.ReleaseWithError(task.ErrLockBusy)
	var cleanup *taskdir.LifetimeOwnerCleanupError
	if !errors.As(cause, &cleanup) || cleanup.Lease() != owner {
		t.Fatalf("missing cleanup owner: %v", cause)
	}
	store, closeErr := closeAdmissionStore(fixture.store, cause)
	if store != fixture.store || closeErr != nil {
		t.Fatalf("Store closed before child: %v", closeErr)
	}
	assertLifetimeCleanupIsConcrete(t, cause)
	fault.enabled.Store(false)
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
	if pendingAdmissionCleanup(cause) {
		t.Fatal("settled child still blocks parent")
	}
	assertLifetimeCleanupIsConcrete(t, cause)
}

func assertLifetimeCleanupIsConcrete(t *testing.T, cause error) {
	t.Helper()
	if IsPureCollectionPendingError(cause) || IsPureSupervisorUncertaintyError(cause) {
		t.Fatal("cleanup became pending-only")
	}
	if !errors.Is(collectionSupervisorError(t.Context(), cause), task.ErrLockBusy) {
		t.Fatal("collection dropped cleanup error")
	}
}
