package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func maintenancePublicFailureFixture(activity string) *supervisorcore.MaintenanceError {
	blocker := &supervisorcore.MaintenanceBlocker{Kind: "task", TaskID: "cccccccccccccccccccccccccccccccc", ReasonCode: "lifetime_closure_unproven", NextAction: "human_review_task"}
	busy := inspection.ErrGroupMigrationBusy
	if activity == "present" {
		blocker = &supervisorcore.MaintenanceBlocker{Kind: "queue", QueueState: pueue.StateRunning, ReasonCode: "queue_work_present", NextAction: "wait_for_legacy_work"}
		busy = pueue.ErrPrivateSupervisorMigrationBusy
	}
	return &supervisorcore.MaintenanceError{Assessment: supervisorcore.MaintenanceAssessment{Readiness: "blocked", ActiveWork: activity, FirstBlocker: blocker}, Cause: busy}
}

func TestSupervisorMaintenancePublicFailureDoesNotInventQueueWait(t *testing.T) {
	cleanup := &taskdir.LifetimeOwnerCleanupError{}
	if !taskdir.CleanupFault(cleanup) {
		t.Fatal("fixture lost the typed storage cleanup fault")
	}
	for _, fixture := range []struct {
		name, activity string
		fault          error
		faultInside    bool
	}{
		{"closure_only", "none", nil, false},
		{"identity_fault", "none", task.ErrIdentityMismatch, false},
		{"storage_fault_with_queue_activity", "present", os.ErrPermission, false},
		{"storage_fault_inside_assessment", "none", os.ErrPermission, true},
		{"cleanup_fault", "none", cleanup, false},
		{"canceled", "none", context.Canceled, false},
		{"deadline", "none", context.DeadlineExceeded, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			diagnostic := maintenancePublicFailureFixture(fixture.activity)
			busy := diagnostic.Cause
			var cause error = diagnostic
			if fixture.faultInside {
				diagnostic.Cause = errors.Join(busy, fixture.fault)
			} else if fixture.fault != nil {
				cause = errors.Join(diagnostic, fixture.fault)
			}
			result := supervisorMigrationFailure(newSupervisorCommandResponse("migrate"), cause)
			control := result.response.SupervisorControl
			if result.code != 1 || result.err == nil {
				t.Fatalf("maintenance refusal did not remain a failure: %+v", result)
			}
			if control.ReasonCode == "active_work" || strings.Contains(result.err.Error(), "Wait for queued or running work") {
				t.Errorf("maintenance assessment became false queue guidance: control=%+v error=%q", control, result.err.Error())
			}
			if control.ActiveWork != fixture.activity {
				t.Errorf("public reason changed independently observed queue activity: %+v", control)
			}
			wantAction := "human_review_task"
			if fixture.fault != nil {
				wantAction = "stop_and_report"
			}
			if control.NextAction != wantAction {
				t.Errorf("maintenance action=%q want=%q", control.NextAction, wantAction)
			}
			assertMaintenancePublicCauses(t, result.err, diagnostic, busy, fixture.fault)
		})
	}
}

func TestSupervisorMaintenancePublicQueueWaitKeepsLegacyCompatibility(t *testing.T) {
	activity := supervisorcore.QueueActivity([]pueue.Job{{ID: 7, State: pueue.StateRunning}})
	if activity != "present" || infrastructure.PreparationReasonActiveWork.Validate() != nil {
		t.Fatal("fixture requires positive queue activity and the existing active_work recovery reason")
	}
	for _, legacy := range []bool{false, true} {
		diagnostic := maintenancePublicFailureFixture(activity)
		var cause error = diagnostic
		if legacy {
			cause = &pueue.PrivateRecoveryError{ReasonCode: string(infrastructure.PreparationReasonActiveWork), Err: diagnostic}
		}
		result := supervisorMigrationFailure(newSupervisorCommandResponse("migrate"), cause)
		control := result.response.SupervisorControl
		if result.code != 1 || result.err == nil || control.ActiveWork != "present" || control.ReasonCode != "active_work" || control.NextAction != "wait_for_legacy_work" || !strings.Contains(result.err.Error(), "Wait for queued or running work before migrating.") {
			t.Fatalf("positive queue activity lost its existing guidance: legacy=%t control=%+v error=%v", legacy, control, result.err)
		}
		assertMaintenancePublicCauses(t, result.err, diagnostic, diagnostic.Cause, nil)
	}
}

func TestSupervisorMaintenancePublicLegacyQueueReasonCannotHideFault(t *testing.T) {
	if infrastructure.PreparationReasonActiveWork.Validate() != nil {
		t.Fatal("fixture requires the existing active_work recovery reason")
	}
	diagnostic := maintenancePublicFailureFixture("present")
	recovery := &pueue.PrivateRecoveryError{ReasonCode: string(infrastructure.PreparationReasonActiveWork), Err: diagnostic}
	result := supervisorMigrationFailure(newSupervisorCommandResponse("migrate"), errors.Join(recovery, os.ErrPermission))
	if result.err == nil {
		t.Fatal("joined storage fault did not remain a public failure")
	}
	if result.response.SupervisorControl.ReasonCode == "active_work" || strings.Contains(result.err.Error(), "Wait for queued or running work") {
		t.Error("legacy recovery reason hid the independent storage fault in public guidance")
	}
	assertMaintenancePublicCauses(t, result.err, diagnostic, diagnostic.Cause, os.ErrPermission)
}

func TestSupervisorMaintenancePublicJSONKeepsQueueAndClosureSeparate(t *testing.T) {
	for _, fixture := range []struct {
		name, activity, reason, next, message string
		fault                                 error
	}{
		{"closure", "none", "", "human_review_task", "Supervisor maintenance is blocked.", nil},
		{"identity_fault", "none", "", "stop_and_report", "Delegate could not safely migrate the legacy supervisor.", task.ErrIdentityMismatch},
		{"queue_activity", "present", "active_work", "wait_for_legacy_work", "Supervisor maintenance is blocked. Wait for queued or running work before migrating.", nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			diagnostic := maintenancePublicFailureFixture(fixture.activity)
			cause := errors.Join(diagnostic, fixture.fault)
			result := supervisorMigrationFailure(newSupervisorCommandResponse("migrate"), cause)
			var output bytes.Buffer
			if err := writeJSONResponse(&output, "supervisor", &result.response, result.err); err != nil {
				t.Fatal(err)
			}
			var public struct {
				Error   string `json:"error"`
				Control struct {
					Activity string `json:"active_work"`
					Reason   string `json:"reason_code"`
					Next     string `json:"next_action"`
				} `json:"supervisor_control"`
			}
			if err := json.Unmarshal(output.Bytes(), &public); err != nil {
				t.Fatal(err)
			}
			if public.Error != fixture.message || public.Control.Activity != fixture.activity || public.Control.Reason != fixture.reason || public.Control.Next != fixture.next {
				t.Fatalf("wrong public maintenance projection: %s", output.Bytes())
			}
			assertMaintenancePublicCauses(t, result.err, diagnostic, diagnostic.Cause, fixture.fault)
		})
	}
}

func TestSupervisorMaintenancePublicDetailCompletesBaseSentence(t *testing.T) {
	diagnostic := maintenancePublicFailureFixture("present")
	for _, message := range []string{"Supervisor recovery failed", "Supervisor recovery failed."} {
		public := publicSupervisorError(message, diagnostic)
		if public.Error() != "Supervisor recovery failed. Wait for queued or running work before migrating." {
			t.Fatalf("base and detail did not form complete sentences: %q", public.Error())
		}
		assertMaintenancePublicCauses(t, public, diagnostic, diagnostic.Cause, nil)
	}
}

func assertMaintenancePublicCauses(t *testing.T, err error, diagnostic *supervisorcore.MaintenanceError, busy, fault error) {
	t.Helper()
	if !errors.Is(err, busy) || (fault != nil && !errors.Is(err, fault)) {
		t.Fatal("public error lost a cause from the maintenance graph")
	}
	var retained *supervisorcore.MaintenanceError
	if !errors.As(err, &retained) || retained != diagnostic {
		t.Fatal("public error lost the exact typed maintenance assessment")
	}
}
