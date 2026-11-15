package supervisor

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestMaintenanceEvidenceFaultKeepsQueueOnlyActivity(t *testing.T) {
	for _, fixture := range []struct {
		name            string
		cause           error
		readiness, next string
	}{
		{"pending closure", taskMaintenancePending("cccccccccccccccccccccccccccccccc", "lifetime_closure_unproven", errors.Join(inspection.ErrGroupMigrationBusy, guardian.ErrProofPending)), "blocked", "human_review_task"},
		{"unreadable closure", os.ErrPermission, "unknown", "stop_and_report"},
		{"pending joined storage fault", errors.Join(taskMaintenancePending("cccccccccccccccccccccccccccccccc", "lifetime_closure_unproven", inspection.ErrGroupMigrationBusy), os.ErrPermission), "unknown", "stop_and_report"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			got, err := classifyMaintenance([]maintenanceFact{{job: pueue.Job{ID: 7, State: pueue.StateEnded}, taskID: "cccccccccccccccccccccccccccccccc", err: fixture.cause}})
			if got.ActiveWork != "none" || got.Readiness != fixture.readiness || got.FirstBlocker == nil || got.FirstBlocker.NextAction != fixture.next || !errors.Is(err, fixture.cause) {
				t.Fatalf("assessment=%+v err=%v", got, err)
			}
		})
	}
}

func TestMaintenanceCompleteScanDoesNotHideLaterFault(t *testing.T) {
	got, err := classifyMaintenance([]maintenanceFact{
		{job: pueue.Job{ID: 7, State: pueue.StateRunning}},
		{job: pueue.Job{ID: 8, State: pueue.StateEnded}, err: os.ErrPermission},
	})
	if got.Readiness != "unknown" || got.ActiveWork != "present" || got.FirstBlocker.ReasonCode != "evidence_unreadable" || !errors.Is(err, os.ErrPermission) || !errors.Is(err, pueue.ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("assessment=%+v err=%v", got, err)
	}
}

func TestMaintenanceUnknownQueueDoesNotClaimActivity(t *testing.T) {
	got, err := classifyMaintenance([]maintenanceFact{{job: pueue.Job{ID: 7, State: pueue.StateUnknown}}})
	if got.Readiness != "unknown" || got.ActiveWork != "unknown" || got.FirstBlocker.ReasonCode != "queue_state_unknown" || !errors.Is(err, pueue.ErrUnknown) {
		t.Fatalf("assessment=%+v err=%v", got, err)
	}
}

func TestMaintenanceInterruptedScanRetainsEarlierFault(t *testing.T) {
	got, err := interruptedMaintenance(context.Canceled, []maintenanceFact{{job: pueue.Job{ID: 7, State: pueue.StateEnded}, err: os.ErrPermission}}, []pueue.Job{{ID: 7, State: pueue.StateEnded}})
	if got.Readiness != "unknown" || got.ActiveWork != "none" || !errors.Is(err, os.ErrPermission) || !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption lost fault: %+v %v", got, err)
	}
}
