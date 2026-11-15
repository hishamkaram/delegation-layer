package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

func TestSupervisorUnknownQueueStateIsNotObservedActivity(t *testing.T) {
	if got := legacyActiveWork(pueue.QueueSnapshot{Jobs: []pueue.Job{{ID: 7, State: pueue.StateUnknown}}}); got != "unknown" {
		t.Fatalf("unknown queue state reported as observed activity: %q", got)
	}
}

func TestSupervisorClosureBlockerDoesNotInventActiveWork(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	td, _ := lifecyclefixture.NewAppTaskInStoreWithSupervisorConfig(t, store, "cccccccccccccccccccccccccccccccc", false, nil, pueue.PrivateConfigPath(store.Root))
	meta := lifecyclefixture.PrepareAppBudgetRecords(t, td)
	label := "delegate:" + store.RootID + ":" + td.TaskID
	job := pueue.Job{ID: 7, Label: &label, State: pueue.StateEnded, Group: "default"}
	// Production migration invokes this policy under the actual bootstrap EX.
	// Model that prerequisite; the observer itself never creates a lease inode.
	cause := pueue.WithBootstrapLockContext(t.Context(), store.Root, func(locked context.Context) error {
		return authorizePrivateMigrationQueue(locked, store.Root, meta.SupervisorConfig, []pueue.Job{job})
	})
	if !errors.Is(cause, inspection.ErrGroupMigrationBusy) {
		t.Fatalf("fixture has no closure-only busy refusal: %v", cause)
	}
	result := supervisorMigrationFailure(newSupervisorCommandResponse("migrate"), cause)
	control := result.response.SupervisorControl
	if control.ActiveWork != "none" || control.NextAction != "human_review_task" {
		t.Fatalf("closure-only refusal invented active work: %+v", control)
	}
	encoded, err := json.Marshal(control)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["migration_readiness"]) != `"blocked"` {
		t.Fatalf("readiness not blocked: %s", encoded)
	}
	var blocker struct {
		TaskID     string `json:"task_id"`
		ReasonCode string `json:"reason_code"`
	}
	if err = json.Unmarshal(fields["first_blocker"], &blocker); err != nil {
		t.Fatal(err)
	}
	if blocker.TaskID != td.TaskID || blocker.ReasonCode != "lifetime_closure_unproven" {
		t.Fatalf("missing exact blocker: %s", encoded)
	}
}

func TestSupervisorStatusAndMigrationNameSameClosureBlocker(t *testing.T) {
	fixture := newPublishedConfigDriftFixture(t)
	writeEndedHistoricalMigrationStatus(t, fixture)
	deps := withSupervisorProbeRouting(t, fixture.dependencies(), fixture.component)
	before := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID)
	status := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
	assertSupervisorClosureDiagnostic(t, status, fixture.req.TaskID)
	if status.code != 0 || status.err != nil {
		t.Fatalf("passive status failed: code=%d err=%v", status.code, status.err)
	}
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(before, after) {
		t.Fatal("status repaired or changed task evidence")
	}
	migration := supervisorMigrate(fixture.root, deps, newSupervisorCommandResponse("migrate"))
	assertSupervisorClosureDiagnostic(t, migration, fixture.req.TaskID)
	if migration.code != 1 || migration.err == nil || migration.response.SupervisorControl.DiagnosticSnapshotAt == "" {
		t.Fatalf("migration lost refusal or later snapshot: control=%+v err=%v", migration.response.SupervisorControl, migration.err)
	}
	if after := snapshotOutputUpgradeTask(t, fixture.root, fixture.req.TaskID); !equalOutputUpgradeSnapshots(before, after) {
		t.Fatal("refused migration changed task evidence")
	}
}

func assertSupervisorClosureDiagnostic(t *testing.T, result commandResult, taskID string) {
	t.Helper()
	control := result.response.SupervisorControl
	if control.ActiveWork != "none" || control.MigrationReadiness != "blocked" || control.NextAction != "human_review_task" || control.FirstBlocker == nil || control.FirstBlocker.TaskID != taskID || control.FirstBlocker.ReasonCode != "lifetime_closure_unproven" {
		t.Fatalf("closure diagnostic disagrees: control=%+v blocker=%+v err=%v", control, control.FirstBlocker, result.err)
	}
}

func TestSupervisorMigrationJoinedFaultRetainsStopGuidance(t *testing.T) {
	blocker := &supervisorcore.MaintenanceBlocker{Kind: "task", TaskID: "cccccccccccccccccccccccccccccccc", ReasonCode: "lifetime_closure_unproven", NextAction: "human_review_task"}
	pending := &supervisorcore.MaintenanceError{Assessment: supervisorcore.MaintenanceAssessment{Readiness: "blocked", ActiveWork: "none", FirstBlocker: blocker}, Cause: inspection.ErrGroupMigrationBusy}
	result := supervisorMigrationFailure(newSupervisorCommandResponse("migrate"), errors.Join(pending, os.ErrPermission))
	applyMaintenanceGuidance(result.response.SupervisorControl)
	control := result.response.SupervisorControl
	if control.ActiveWork != "none" || control.MigrationReadiness != "unknown" || control.NextAction != "stop_and_report" || control.FirstBlocker == nil || control.FirstBlocker.NextAction != "stop_and_report" || !errors.Is(result.err, os.ErrPermission) {
		t.Fatalf("joined fault hidden: control=%+v blocker=%+v err=%v", control, control.FirstBlocker, result.err)
	}
}

func TestSupervisorLaterDiagnosticFaultOverridesOldBusyGuidance(t *testing.T) {
	original := pueue.ErrPrivateSupervisorMigrationBusy
	report := infrastructure.ManualMigrationReport{State: infrastructure.ManualMigrationBlocked, Legacy: infrastructure.ManualMigrationLegacyUnknown, Active: infrastructure.ManualMigrationActivePresent}
	result := finishRoutedSupervisorMigration(newSupervisorCommandResponse("migrate"), report, original)
	response := newSupervisorCommandResponse("status")
	response.SupervisorControl.ObservedAt = "2026-11-15T00:00:00Z"
	response.SupervisorControl.FirstBlocker = &supervisorcore.MaintenanceBlocker{Kind: "evidence", ReasonCode: "evidence_unreadable", NextAction: "stop_and_report"}
	got := finishSupervisorDiagnostic(result, commandResult{response: response, code: 1, err: os.ErrPermission}, original)
	control := got.response.SupervisorControl
	if control.NextAction != "stop_and_report" || control.MigrationReadiness != "unknown" || control.DiagnosticSnapshotAt == "" || !errors.Is(got.err, original) || !errors.Is(got.err, os.ErrPermission) {
		t.Fatalf("later fault was hidden by old busy: %+v %v", control, got.err)
	}
}

func TestSupervisorDiagnosticCorruptBindingIsNotAbsence(t *testing.T) {
	store := newBareInspectionStore(t)
	directory := filepath.Dir(pueue.PrivateConfigPath(store.Root))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "supervisor.identity.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := supervisorMaintenanceDiagnostic(t.Context(), store, Dependencies{}, pueue.Options{})
	if result.code != 1 || !errors.Is(result.err, pueue.ErrBinding) || errors.Is(result.err, pueue.ErrPrivateSupervisorNotInitialized) || result.response.SupervisorControl.NextAction != "stop_and_report" {
		t.Fatalf("corrupt existing binding became absence: control=%+v err=%v", result.response.SupervisorControl, result.err)
	}
}
