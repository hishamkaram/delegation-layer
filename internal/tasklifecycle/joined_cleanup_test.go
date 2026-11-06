package tasklifecycle

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func joinedAdmissionFailure(t *testing.T, kind string) (func() error, error) {
	t.Helper()
	if kind == "workspace" {
		fixture, _, admission := refusedWorkspaceAdmission(t)
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
	for _, kind := range []string{"lifetime", "workspace"} {
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
	store, closeErr := closeAdmissionStore(fixture.store, cause)
	if store != fixture.store || closeErr != nil {
		t.Fatal("pending child lost its Store")
	}
	if _, err := store.PinRootContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := settle(); err != nil {
		t.Fatal(err)
	}
	if pendingAdmissionCleanup(cause) || !admissionCleanupFault(cause) || IsPureCollectionPendingError(errors.Join(task.ErrNoSeal, cause)) {
		t.Fatal("settled cleanup kept custody or lost historical concrete-fault classification")
	}
	if _, err := closeAdmissionStore(store, cause); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatal("settled historical fault retained Store")
	}
}
