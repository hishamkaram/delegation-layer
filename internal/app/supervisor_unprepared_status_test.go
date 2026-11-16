package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/provider/ccr"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Completed private history is validated DATA. The component owns only the
// current script daemon; no historical process birth or closure is claimed.
func TestUnpreparedSupervisorStatusNamesRequiredInspectionMigration(t *testing.T) {
	for _, earlierEdge := range []bool{false, true} {
		name := "initial inspection head"
		if earlierEdge {
			name = "previously migrated inspection head"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newUnpreparedSupervisorStatusFixture(t)
			legacy := seedUnpreparedSupervisorStatusHead(t, fixture, earlierEdge)
			assertUnpreparedSupervisorStatusConsumers(t, fixture, legacy)
			deps := fixture.dependencies()
			deps.InitialSupervisorExecutable = fixture.clientA
			before := snapshotMaintenanceStatusRoot(t, fixture.root)
			result := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
			control := result.response.SupervisorControl
			if result.code != 0 || result.err != nil || control.State != "legacy_present" || control.NextAction != "run_supervisor_migrate" || control.MigrationPhase != "" {
				t.Errorf("status concealed the same required inspection migration: code=%d control=%+v err=%v", result.code, control, result.err)
			}
			if control.ObservedAt == "" || control.MigrationReadiness != "clear" || control.ActiveWork != "none" || control.FirstBlocker != nil {
				t.Errorf("status lost its actual empty-queue maintenance observation: %+v", control)
			}
			assertMaintenanceStatusRootUnchanged(t, fixture.root, before)
			completeUnpreparedSupervisorStatusMigration(t, fixture, deps)
		})
	}
}

func TestUnpreparedSupervisorStatusKeepsExactPreparedEdgeReadOnly(t *testing.T) {
	fixture := newUnpreparedSupervisorStatusFixture(t)
	legacy := seedUnpreparedSupervisorStatusHead(t, fixture, false)
	if err := inspection.PrepareGroupMigration(t.Context(), fixture.store, supervisorcore.InspectionGroup(fixture.store.RootID, legacy), supervisorcore.InspectionGroup(fixture.store.RootID, fixture.current.Binding())); err != nil {
		t.Fatal(err)
	}
	deps := fixture.dependencies()
	deps.InitialSupervisorExecutable = fixture.clientA
	before := snapshotMaintenanceStatusRoot(t, fixture.root)
	result := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
	control := result.response.SupervisorControl
	if result.code != 0 || result.err != nil || control.State != "legacy_present" || control.NextAction != "run_supervisor_migrate" || control.MigrationPhase != "" {
		t.Errorf("status treated preparation as completed routing: code=%d control=%+v err=%v", result.code, control, result.err)
	}
	assertMaintenanceStatusRootUnchanged(t, fixture.root, before)
	completeUnpreparedSupervisorStatusMigration(t, fixture, deps)
}

func newUnpreparedSupervisorStatusFixture(t *testing.T) *historicalSupervisorFixture {
	t.Helper()
	fixture := newHistoricalSupervisorFixturePaths(t)
	prepareHistoricalSupervisorExecutables(t, fixture)
	fixture.options = newHistoricalSupervisorOptions()
	bindHistoricalSupervisorFixture(t, fixture)
	status := `{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0},"delegation-inspection-` + fixture.store.RootID + `":{"status":"Running","parallel_tasks":1}}}`
	if err := os.WriteFile(fixture.statusPath, []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	preparePrivateMaintenanceStatusObservation(t, fixture)
	ids, err := fixture.store.TaskIDs(t.Context())
	if err != nil || len(ids) != 0 {
		t.Fatalf("status fixture unexpectedly owns task metadata: ids=%v err=%v", ids, err)
	}
	return fixture
}

func seedUnpreparedSupervisorStatusHead(t *testing.T, fixture *historicalSupervisorFixture, earlierEdge bool) task.SupervisorRef {
	t.Helper()
	current := fixture.current.Binding()
	legacy := current
	legacy.ConfigDigest = task.ComputeSHA256([]byte("completed private predecessor config"))
	legacy.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("completed private predecessor resolved config"))
	writeSupervisorGenerationTestHistory(t, fixture.store, legacy, current)
	if earlierEdge {
		original := legacy
		original.ConfigDigest = task.ComputeSHA256([]byte("earlier completed inspection config"))
		writeSupervisorGenerationTestGroup(t, fixture.store, original)
		publishAppTestGroupMigration(t, fixture.store, supervisorcore.InspectionGroup(fixture.store.RootID, original), supervisorcore.InspectionGroup(fixture.store.RootID, legacy))
	} else {
		writeSupervisorGenerationTestGroup(t, fixture.store, legacy)
	}
	resolved, err := resolveSupervisorGenerationTestWithResult(t, fixture.store, legacy)
	if err != nil || !resolved.PrivateAuthority || !resolved.GroupCleanupDue || resolved.Active != current || resolved.GroupMigrated != earlierEdge {
		t.Fatalf("fixture lacks the exact stale inspection boundary: resolution=%+v err=%v", resolved, err)
	}
	prepared, err := inspection.PreparedGroupMigrationSuccessors(t.Context(), fixture.store)
	if err != nil || len(prepared) != 0 {
		t.Fatalf("fixture already prepared a current inspection target: prepared=%v err=%v", prepared, err)
	}
	return legacy
}

func assertUnpreparedSupervisorStatusConsumers(t *testing.T, fixture *historicalSupervisorFixture, legacy task.SupervisorRef) {
	t.Helper()
	before := snapshotMaintenanceStatusRoot(t, fixture.root)
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		ready, validateErr := supervisorcore.RevalidateContinuationSuccessor(ctx, fixture.root, legacy, func(openContext context.Context, root string) (*taskdir.Store, error) {
			return taskdir.OpenExistingStoreMetadataContext(openContext, root, predicate.Default())
		})
		if ready != (task.SupervisorRef{}) {
			t.Error("unprepared continuation returned a ready successor")
		}
		return validateErr
	})
	if !errors.Is(err, inspection.ErrGroupMigrationRequired) || errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("continuation did not retain its honest migration refusal: %v", err)
	}
	err = supervisorcore.EnsureInspectionGroupWithMigrationContext(t.Context(), fixture.store, fixture.current, 10*time.Second, false)
	if !errors.Is(err, inspection.ErrGroupMigrationRequired) || errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("inspection admission did not retain its honest migration refusal: %v", err)
	}
	assertMaintenanceStatusRootUnchanged(t, fixture.root, before)
}

func completeUnpreparedSupervisorStatusMigration(t *testing.T, fixture *historicalSupervisorFixture, deps Dependencies) {
	t.Helper()
	history := filepath.Join(fixture.root, ".supervisor", "migration-history.json")
	privateBefore := readSupervisorProbeTestFile(t, history)
	starts := readSupervisorProbeTestFile(t, fixture.daemonStartLog)
	result := supervisorMigrate(fixture.root, withSupervisorProbeRouting(t, deps, fixture.component), newSupervisorCommandResponse("migrate"))
	if result.code != 0 || result.err != nil || result.response.SupervisorControl.State != "migrated" {
		t.Fatalf("explicit migration could not complete its inspection edge: code=%d control=%+v err=%v", result.code, result.response.SupervisorControl, result.err)
	}
	head, migrated, err := inspection.ReadGroupHead(t.Context(), fixture.store)
	if err != nil || !migrated || head.Supervisor != fixture.current.Binding() {
		t.Fatalf("explicit migration did not publish the current inspection head: head=%+v migrated=%t err=%v", head, migrated, err)
	}
	if !bytes.Equal(privateBefore, readSupervisorProbeTestFile(t, history)) || !bytes.Equal(starts, readSupervisorProbeTestFile(t, fixture.daemonStartLog)) {
		t.Fatal("inspection-only completion repeated the private migration or daemon Start")
	}
	before := snapshotMaintenanceStatusRoot(t, fixture.root)
	status := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
	if status.code != 0 || status.err != nil || status.response.SupervisorControl.State != "migrated" || status.response.SupervisorControl.NextAction != "none" {
		t.Fatalf("current completed inspection head still required migration: code=%d control=%+v err=%v", status.code, status.response.SupervisorControl, status.err)
	}
	assertMaintenanceStatusRootUnchanged(t, fixture.root, before)
	assertCompletedSupervisorStatusConsumers(t, fixture)
	if !bytes.Equal(privateBefore, readSupervisorProbeTestFile(t, history)) || !bytes.Equal(starts, readSupervisorProbeTestFile(t, fixture.daemonStartLog)) {
		t.Fatal("completed shared consumers repeated the private migration or daemon Start")
	}
}

func assertCompletedSupervisorStatusConsumers(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		ready, validateErr := supervisorcore.RevalidateContinuationSuccessor(ctx, fixture.root, fixture.current.Binding(), func(openContext context.Context, root string) (*taskdir.Store, error) {
			return taskdir.OpenExistingStoreMetadataContext(openContext, root, predicate.Default())
		})
		if ready != fixture.current.Binding() {
			t.Error("completed continuation did not return the current successor")
		}
		return validateErr
	})
	if err != nil {
		t.Fatalf("completed inspection edge still refused continuation: %v", err)
	}
	if err = supervisorcore.EnsureInspectionGroupWithMigrationContext(t.Context(), fixture.store, fixture.current, 10*time.Second, false); err != nil {
		t.Fatalf("completed inspection edge still refused serial admission: %v", err)
	}
}

func TestUnpreparedSupervisorStatusPreservesMaintenanceAndFaultGuidance(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	preparePrivateMaintenanceStatusObservation(t, fixture)
	writeEndedHistoricalMigrationStatus(t, fixture)
	recordSettlementLifetime(t, fixture.td, nil)
	seedUnpreparedSupervisorStatusHead(t, fixture, false)
	deps := fixture.dependencies()
	deps.InitialSupervisorExecutable = fixture.clientA
	before := snapshotMaintenanceStatusRoot(t, fixture.root)
	healthy := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
	assertDatedStatusClosureBlocker(t, healthy.response.SupervisorControl, fixture.req.TaskID)
	if healthy.code != 0 || healthy.err != nil || healthy.response.SupervisorControl.NextAction != "human_review_task" {
		t.Fatalf("inspection repair advice displaced the actual maintenance blocker: code=%d control=%+v err=%v", healthy.code, healthy.response.SupervisorControl, healthy.err)
	}
	assertMaintenanceStatusRootUnchanged(t, fixture.root, before)
	if err := os.WriteFile(filepath.Join(fixture.root, ".supervisor", "ordinary-scheduling.json"), []byte(`{"schema_version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	before = snapshotMaintenanceStatusRoot(t, fixture.root)
	failed := supervisorStatus(fixture.root, deps, newSupervisorCommandResponse("status"))
	assertDatedStatusClosureBlocker(t, failed.response.SupervisorControl, fixture.req.TaskID)
	if failed.code != 1 || !errors.Is(failed.err, pueue.ErrBinding) || failed.response.SupervisorControl.NextAction != "stop_and_report" {
		t.Fatalf("inspection repair advice concealed a later scheduling fault: code=%d control=%+v err=%v", failed.code, failed.response.SupervisorControl, failed.err)
	}
	assertMaintenanceStatusRootUnchanged(t, fixture.root, before)
}

// Current CCR candidates carry a native route projection and use the serial
// inspection path. This is the actual admission boundary called by dispatch;
// the isolated provider must never execute while that group needs migration.
func TestUnpreparedSupervisorStatusHasCurrentCCRAdmissionConsumer(t *testing.T) {
	fixture := newUnpreparedSupervisorStatusFixture(t)
	seedUnpreparedSupervisorStatusHead(t, fixture, false)
	base := filepath.Dir(fixture.root)
	providerPath := filepath.Join(base, "provider-bin", "ccr")
	invoked := filepath.Join(base, "provider-invoked")
	writeRunnerFixture(t, providerPath, "#!/bin/sh\nprintf invoked >> "+shellQuote(invoked)+"\nexit 97\n")
	request, err := admission.BuildRequest(fixture.store.RootID, strings.Repeat("e", 32), ccr.Provider, filepath.Join(base, "workspace"), task.TaskConfig{Model: "recorded-alias", Permission: "read-only", Budget: "1m0s"}, []byte("inspection migration probe"), nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := ccr.PrepareCandidate(request, []string{"HOME=" + base, "PATH=" + filepath.Dir(providerPath) + ":/usr/bin:/bin"})
	if err != nil || candidate.Inspection == nil || candidate.Inspection.RuntimeOnly() || candidate.Admit != nil {
		t.Fatalf("current CCR did not select its serial admission contract: err=%v", err)
	}
	before := snapshotMaintenanceStatusRoot(t, fixture.root)
	facts, _, inspectErr := admission.InspectProviderContext(t.Context(), admission.InspectionOptions{Runner: fixture.runner, RunnerOwnership: task.RunnerOwnershipCustom}, fixture.store, request, candidate, fixture.current, false)
	if !errors.Is(inspectErr, inspection.ErrGroupMigrationRequired) || errors.Is(inspectErr, task.ErrEvidenceFault) || facts != nil {
		t.Fatalf("current CCR admission did not require the same inspection migration: facts=%s err=%v", facts, inspectErr)
	}
	failure := dispatchFailure(admission.WithStage("inspect-provider", inspectErr), "not_created")
	if failure.Code != "supervisor_upgrade_required" || failure.NextAction != "run_supervisor_migrate" || failure.RecoveryCommand != "delegate supervisor migrate --root <same-state-root> --json" {
		t.Fatalf("current CCR admission lost its existing dispatch repair guidance: %+v", failure)
	}
	if _, statErr := os.Stat(invoked); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unprepared group launched the provider: %v", statErr)
	}
	assertMaintenanceStatusRootUnchanged(t, fixture.root, before)
}
