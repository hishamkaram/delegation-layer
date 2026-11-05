package tasklifecycle

import (
	"errors"
	"fmt"
	"os"
	"testing"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func joinedAdmissionFailure(t *testing.T, kind string) (func() error, error) {
	t.Helper()
	if kind == "workspace" {
		fixture, _, _, admission := refusedWorkspaceAdmission(t)
		fault := &admissionReleaseFault{name: ".lifetime-owner.lock", cause: errors.New("workspace close failed")}
		fault.enabled.Store(true)
		fixture.Store.SetFaultInjector(fault)
		cause := admission.Close()
		fault.enabled.Store(false)
		return admission.Close, cause
	}
	fixture := newOrdinaryAdmissionFixture(t)
	fault := &admissionReleaseFault{cause: errors.New("native close failed")}
	fixture.store.SetFaultInjector(fault)
	if kind == "root" {
		lease, err := fixture.store.AcquireRootAdmissionContext(t.Context())
		fixture.own(lease)
		if err != nil {
			t.Fatal(err)
		}
		fault.enabled.Store(true)
		cause := supervisorcore.ReleaseOwnedRootAdmission(lease, nil)
		fault.enabled.Store(false)
		return lease.Release, cause
	}
	control, err := fixture.store.OpenInfrastructureContext(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := control.AcquireLifetimeOwner(t.Context())
	t.Cleanup(func() {
		fault.enabled.Store(false)
		if closeErr := errors.Join(lease.Release(), control.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	fault.name = ".maintenance.lock"
	fault.enabled.Store(true)
	cause := lease.ReleaseWithError(errors.New("lifetime work failed"))
	fault.enabled.Store(false)
	return lease.Release, cause
}

func TestJoinedAdmissionFinalizersRetainEveryPendingKind(t *testing.T) {
	for _, kind := range []string{"root", "lifetime", "workspace"} {
		for _, order := range []string{"settled-first", "pending-first"} {
			t.Run(kind+"/"+order, func(t *testing.T) {
				settleFirst, historical := joinedAdmissionFailure(t, kind)
				if err := settleFirst(); err != nil {
					t.Fatal(err)
				}
				settleSecond, pending := joinedAdmissionFailure(t, kind)
				cause := errors.Join(historical, fmt.Errorf("wrapped pending: %w", pending))
				if order == "pending-first" {
					cause = errors.Join(pending, fmt.Errorf("wrapped historical: %w", historical))
				}
				// Dispatch's real error projection preserves multiple typed branches.
				projected := DispatchResult{Failure: cause, Workspace: &workspaces.AdmissionFinish{CloseError: pending}}
				assertJoinedAdmissionFinalizers(t, projected.Err(), settleSecond)
			})
		}
	}
}

func assertJoinedAdmissionFinalizers(t *testing.T, cause error, settle func() error) {
	t.Helper()
	fixture := newOrdinaryAdmissionFixture(t)
	lease, err := fixture.store.AcquireRootAdmissionContext(t.Context())
	fixture.own(lease)
	if err != nil {
		t.Fatal(err)
	}
	retained, releaseErr := releaseRootAdmission(lease, cause)
	if retained != lease || releaseErr != nil || !lease.CleanupPending() {
		t.Fatal("historical first error hid a pending child from parent finalizer")
	}
	store, closeErr := closeAdmissionStore(fixture.store, nil, cause)
	if store != fixture.store || closeErr != nil {
		t.Fatal("pending child lost its Store")
	}
	if _, err = store.PinRootContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = settle(); err != nil {
		t.Fatal(err)
	}
	if pendingAdmissionCleanup(cause) || !admissionCleanupFault(cause) || IsPureCollectionPendingError(errors.Join(task.ErrNoSeal, cause)) {
		t.Fatal("settled cleanup kept custody or lost historical concrete-fault classification")
	}
	if _, err = releaseRootAdmission(lease, cause); err != nil {
		t.Fatal(err)
	}
	if _, err = closeAdmissionStore(store, nil, cause); err != nil {
		t.Fatal(err)
	}
	if _, err = store.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatal("settled historical fault retained Store")
	}
}
