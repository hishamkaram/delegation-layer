package app

import (
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/host"
	schedulerruntime "github.com/hishamkaram/delegation-layer/internal/scheduler/runtime"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestSchedulerCleanupClassificationPrecedesStorageIncompatibility(t *testing.T) {
	for _, fixture := range []struct {
		name         string
		cause        error
		resident     bool
		cleanup      bool
		incompatible bool
	}{
		{"cleanup", schedulerruntime.ErrTaskCleanupUnresolved, true, true, false},
		{"cleanup with invariant", errors.Join(schedulerruntime.ErrTaskCleanupUnresolved, task.ErrInvariantFault), true, true, false},
		{"cleanup with schema", errors.Join(schedulerruntime.ErrTaskCleanupUnresolved, task.ErrUnsupportedSchema), true, true, false},
		{"ordinary invariant", task.ErrInvariantFault, true, false, true},
		{"ordinary fault", os.ErrPermission, true, false, false},
		{"foreground cleanup", schedulerruntime.ErrTaskCleanupUnresolved, false, false, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			result := schedulerRuntimeError(fixture.cause, fixture.resident)
			if errors.Is(result, host.ErrCleanupUnresolved) != fixture.cleanup || errors.Is(result, host.ErrIncompatible) != fixture.incompatible || !errors.Is(result, fixture.cause) {
				t.Fatalf("scheduler lost cleanup precedence or original cause: %v", result)
			}
		})
	}
}

func TestSchedulerCleanupProjectionRetainsStoreCloseOwner(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	projected := dispatchCommandResult(tasklifecycle.DispatchResult{StoreCleanup: store, StoreCloseError: os.ErrPermission})
	marked := schedulerTaskCleanupError(projected.err)
	var cleanup *taskAdmissionCleanupError
	if !errors.As(marked, &cleanup) || cleanup == nil || cleanup.store != store || !errors.Is(marked, schedulerruntime.ErrTaskCleanupUnresolved) {
		t.Fatalf("scheduler lost failed Store-close owner: %v", marked)
	}
	if _, err := store.PinRootContext(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerCleanupOrdinaryAndSettledViewsRemainNonfatal(t *testing.T) {
	for _, cause := range []error{nil, os.ErrPermission, &routingCleanupTestError{cause: os.ErrPermission}, &taskAdmissionCleanupError{cause: os.ErrPermission}, &nilSchedulerRetainedStoreError{cause: os.ErrPermission}} {
		result := schedulerTaskCleanupError(cause)
		if errors.Is(result, schedulerruntime.ErrTaskCleanupUnresolved) {
			t.Fatalf("settled or ordinary task error became fatal: %v", result)
		}
		if cause != nil && !errors.Is(result, cause) {
			t.Fatal("ordinary cause was dropped")
		}
	}
}

type nilSchedulerRetainedStoreError struct{ cause error }

func (e *nilSchedulerRetainedStoreError) Error() string               { return "settled test cleanup view" }
func (e *nilSchedulerRetainedStoreError) Unwrap() error               { return e.cause }
func (*nilSchedulerRetainedStoreError) RetainedStore() *taskdir.Store { return nil }
