package tasklifecycle

import (
	"errors"
	"testing"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSettledRootAdmissionCleanupRemainsConcrete(t *testing.T) {
	fixture := newOrdinaryAdmissionFixture(t)
	lease, err := fixture.store.AcquireRootAdmissionContext(t.Context())
	fixture.own(lease)
	if err != nil {
		t.Fatal(err)
	}
	cause := supervisorcore.RetainOwnedRootAdmission(lease, task.ErrLockBusy)
	if !pendingAdmissionCleanup(cause) {
		t.Fatal("held root lost its cleanup dependency")
	}
	assertLifetimeCleanupIsConcrete(t, cause)
	if err = lease.Release(); err != nil {
		t.Fatal(err)
	}
	if pendingAdmissionCleanup(cause) {
		t.Fatal("settled root retained a historical cleanup dependency")
	}
	assertLifetimeCleanupIsConcrete(t, cause)
}

func TestOuterAdmissionWaitsForLifetimeCleanup(t *testing.T) {
	fixture := newOrdinaryAdmissionFixture(t)
	outer, err := fixture.store.AcquireRootAdmissionContext(t.Context())
	fixture.own(outer)
	if err != nil {
		t.Fatal(err)
	}
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
	retained, releaseErr := releaseRootAdmission(outer, cause)
	if retained != outer || releaseErr != nil {
		t.Fatalf("outer released before child: %v", releaseErr)
	}
	store, closeErr := closeAdmissionStore(fixture.store, retained, cause)
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
	retained, releaseErr = releaseRootAdmission(outer, cause)
	if retained != nil || releaseErr != nil {
		t.Fatalf("settled child blocked release: %v", releaseErr)
	}
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
