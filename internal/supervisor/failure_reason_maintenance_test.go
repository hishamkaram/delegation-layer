package supervisor

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func maintenanceFailureReasonFixture(activity string, busy error) *MaintenanceError {
	blocker := &MaintenanceBlocker{Kind: "task", TaskID: "cccccccccccccccccccccccccccccccc", ReasonCode: "lifetime_closure_unproven", NextAction: "human_review_task"}
	if activity == "present" {
		blocker = &MaintenanceBlocker{Kind: "queue", QueueState: pueue.StateRunning, ReasonCode: "queue_work_present", NextAction: "wait_for_legacy_work"}
	}
	return &MaintenanceError{Assessment: MaintenanceAssessment{Readiness: "blocked", ActiveWork: activity, FirstBlocker: blocker}, Cause: busy}
}

func TestFailureReasonMaintenanceDoesNotInventQueueActivity(t *testing.T) {
	// Typed historical cleanup failures remain faults after resource custody ends.
	cleanup := &taskdir.LifetimeOwnerCleanupError{}
	if !taskdir.CleanupFault(cleanup) {
		t.Fatal("fixture lost the typed storage cleanup fault")
	}
	for _, fixture := range []struct {
		name, activity string
		busy, fault    error
		faultInside    bool
	}{
		{"closure_group_busy", "none", inspection.ErrGroupMigrationBusy, nil, false},
		{"closure_supervisor_busy", "none", pueue.ErrPrivateSupervisorMigrationBusy, nil, false},
		{"identity_fault", "none", inspection.ErrGroupMigrationBusy, task.ErrIdentityMismatch, false},
		{"storage_fault_with_queue_activity", "present", pueue.ErrPrivateSupervisorMigrationBusy, os.ErrPermission, false},
		{"storage_fault_inside_assessment", "none", inspection.ErrGroupMigrationBusy, os.ErrPermission, true},
		{"cleanup_fault", "none", inspection.ErrGroupMigrationBusy, cleanup, false},
		{"canceled", "none", inspection.ErrGroupMigrationBusy, context.Canceled, false},
		{"deadline", "none", inspection.ErrGroupMigrationBusy, context.DeadlineExceeded, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			diagnostic := maintenanceFailureReasonFixture(fixture.activity, fixture.busy)
			var cause error = diagnostic
			if fixture.faultInside {
				diagnostic.Cause = errors.Join(fixture.busy, fixture.fault)
			} else if fixture.fault != nil {
				cause = errors.Join(diagnostic, fixture.fault)
			}
			if got := FailureReason(cause); got == "active_work" {
				t.Errorf("maintenance assessment %+v and fault %v became queue activity", diagnostic.Assessment, fixture.fault)
			}
			if !errors.Is(cause, fixture.busy) || (fixture.fault != nil && !errors.Is(cause, fixture.fault)) {
				t.Fatal("reason projection lost a cause from the error graph")
			}
			var retained *MaintenanceError
			if !errors.As(cause, &retained) || retained != diagnostic {
				t.Fatal("reason projection lost the exact typed maintenance assessment")
			}
		})
	}
}

func TestFailureReasonMaintenanceQueueActivityKeepsLegacyCompatibility(t *testing.T) {
	activity := QueueActivity([]pueue.Job{{ID: 7, State: pueue.StateRunning}})
	if activity != "present" || infrastructure.PreparationReasonActiveWork.Validate() != nil {
		t.Fatal("fixture requires positive queue activity and the existing active_work recovery reason")
	}
	for _, legacy := range []bool{false, true} {
		diagnostic := maintenanceFailureReasonFixture(activity, pueue.ErrPrivateSupervisorMigrationBusy)
		var cause error = diagnostic
		if legacy {
			cause = &pueue.PrivateRecoveryError{ReasonCode: string(infrastructure.PreparationReasonActiveWork), Err: diagnostic}
		}
		if got := FailureReason(cause); got != "active_work" {
			t.Errorf("positive queue activity lost its existing reason: legacy=%t got=%q", legacy, got)
		}
	}
}

func TestFailureReasonMaintenanceLegacyQueueReasonCannotHideFault(t *testing.T) {
	if infrastructure.PreparationReasonActiveWork.Validate() != nil {
		t.Fatal("fixture requires the existing active_work recovery reason")
	}
	diagnostic := maintenanceFailureReasonFixture("present", pueue.ErrPrivateSupervisorMigrationBusy)
	recovery := &pueue.PrivateRecoveryError{ReasonCode: string(infrastructure.PreparationReasonActiveWork), Err: diagnostic}
	mixed := errors.Join(recovery, os.ErrPermission)
	if got := FailureReason(mixed); got == "active_work" {
		t.Error("legacy recovery reason hid an independent storage fault")
	}
	if !errors.Is(mixed, os.ErrPermission) || !errors.Is(mixed, pueue.ErrPrivateSupervisorMigrationBusy) {
		t.Fatal("legacy reason projection lost joined causes")
	}
}
