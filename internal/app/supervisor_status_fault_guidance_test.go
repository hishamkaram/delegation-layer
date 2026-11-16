package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestPublicSupervisorStatusSchedulingFaultPreservesStopGuidance(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	preparePrivateMaintenanceStatusObservation(t, fixture)
	writeEndedHistoricalMigrationStatus(t, fixture)
	recordSettlementLifetime(t, fixture.td, nil)
	deps := fixture.dependencies()
	deps.InitialSupervisorExecutable = fixture.clientA
	before := snapshotMaintenanceStatusRoot(t, fixture.root)
	healthy := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
	assertDatedStatusClosureBlocker(t, healthy.response.SupervisorControl, fixture.req.TaskID)
	if healthy.code != 0 || healthy.err != nil || healthy.response.SupervisorControl.NextAction != "human_review_task" {
		t.Fatalf("healthy closure-only assessment became an operational failure: code=%d control=%+v err=%v", healthy.code, healthy.response.SupervisorControl, healthy.err)
	}
	assertMaintenanceStatusRootUnchanged(t, fixture.root, before)

	scheduling := filepath.Join(fixture.root, ".supervisor", "ordinary-scheduling.json")
	if err := os.WriteFile(scheduling, []byte(`{"schema_version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	before = snapshotMaintenanceStatusRoot(t, fixture.root)
	failed := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
	control := failed.response.SupervisorControl
	if failed.code != 1 || !errors.Is(failed.err, pueue.ErrBinding) || control.NextAction != "stop_and_report" {
		t.Errorf("later scheduling fault lost fail-stop guidance: code=%d control=%+v err=%v", failed.code, control, failed.err)
	}
	assertDatedStatusClosureBlocker(t, control, fixture.req.TaskID)
	assertMaintenanceStatusRootUnchanged(t, fixture.root, before)
}

func assertDatedStatusClosureBlocker(t *testing.T, control *SupervisorControlResponse, taskID string) {
	t.Helper()
	if control.ObservedAt == "" || control.ActiveWork != "none" || control.MigrationReadiness != "blocked" || control.FirstBlocker == nil || control.FirstBlocker.TaskID != taskID || control.FirstBlocker.ReasonCode != "lifetime_closure_unproven" {
		t.Errorf("scheduling fault lost or changed the earlier dated closure blocker: %+v", control)
	}
}
