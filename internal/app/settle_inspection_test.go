package app

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func inspectionSettlementOperation(t *testing.T, f *historicalSupervisorFixture, when time.Time, managed bool) *inspection.Operation {
	t.Helper()
	binding := inspection.Binding{DefinitionRevision: "inspection-v1", DefinitionSHA256: strings.Repeat("a", 64), HelperExecutable: f.runner, WorkerExecutable: f.runner, ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, RunnerOwnership: task.RunnerOwnershipManaged, Supervisor: f.meta.SupervisorConfig}
	if managed {
		binding.WorkerExitProof = inspection.WorkerExitProofGuardianWaitV1
	}
	op, err := inspection.OpenOperation(f.store, f.req, binding, when)
	requireSettlementTest(t, err)
	t.Cleanup(func() { requireSettlementTest(t, op.Close()) })
	return op
}

// Synthetic absent process slots exercise CLI wiring only. Native acceptance
// separately records an owned child's observed identity and actual Wait.
func recordInspectionSettlementLifetime(t *testing.T, op *inspection.Operation) {
	t.Helper()
	recordInspectionSettlementLifetimeKind(t, op, "cooperative")
}

func recordInspectionSettlementLifetimeKind(t *testing.T, op *inspection.Operation, kind string) {
	t.Helper()
	boot, err := lifetime.BootID()
	requireSettlementTest(t, err)
	owner, err := op.AcquireLifetimeOwner(t.Context())
	requireSettlementTest(t, err)
	defer func() { requireSettlementTest(t, owner.Release()) }()
	scope, assurance := task.LifetimeScopeProcessGroup, task.LifetimeAssuranceCooperative
	if kind == "exact" {
		scope, assurance = task.LifetimeScopeCgroupV2, task.LifetimeAssuranceExact
	}
	_, err = op.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{ScopeKind: scope, Assurance: assurance, ScopePlan: "inspection-settlement", BootID: boot, Deadline: op.Deadline()})
	requireSettlementTest(t, err)
	if kind == "intent-only" {
		return
	}
	_, err = op.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{ScopeKind: scope, Assurance: assurance, ScopeID: "inspection-settlement", BootID: boot, Deadline: op.Deadline()})
	requireSettlementTest(t, err)
	self, err := lifetime.ObserveProcess(os.Getpid())
	requireSettlementTest(t, err)
	_, err = op.RecordLifetimeWorker(taskdir.LifetimeWorkerFacts{PID: 1 << 30, ProcessStart: self.StartToken(), BootID: boot})
	requireSettlementTest(t, err)
}

func TestInspectionSettlementCLIReadOnlyPrepareConfirmAndHistoricalWinner(t *testing.T) {
	f := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	writeEndedHistoricalMigrationStatus(t, f)
	op := inspectionSettlementOperation(t, f, time.Now(), true)
	recordInspectionSettlementLifetime(t, op)
	args := Arguments{Root: f.root, TaskID: op.Request().IdentityID(), SettlementKind: "inspection", SettlementPrepare: true}
	deps := f.dependencies()
	before := supervisorfixture.SnapshotStore(t, f.root)
	prepared := settleContext(t.Context(), args, deps)
	if prepared.code != 0 || prepared.response.Settlement == nil {
		t.Fatalf("prepare: code=%d err=%v", prepared.code, prepared.err)
	}
	if !reflect.DeepEqual(before, supervisorfixture.SnapshotStore(t, f.root)) {
		t.Fatal("inspection preparation mutated root")
	}
	response := prepared.response.Settlement
	if response.TargetKind != "inspection" || response.OperationID != args.TaskID || response.NativeExecution != "unknown" || response.Permission != task.OperatorPermissionSupervisorMaintenance {
		t.Fatalf("unexpected prepared inspection response %+v", response)
	}
	args.SettlementPrepare = false
	args.SettlementConfirm = response.Challenge
	confirmed := settleContext(t.Context(), args, deps)
	if confirmed.code != 0 || confirmed.response.Settlement == nil || confirmed.response.Settlement.AdministrativeState != "settled" {
		t.Fatalf("confirm: code=%d err=%v", confirmed.code, confirmed.err)
	}
	deps.InitialSupervisorExecutable = filepath.Join(t.TempDir(), "removed", "pueue")
	repeated := settleContext(t.Context(), args, deps)
	if repeated.code != 0 || !reflect.DeepEqual(confirmed.response.Settlement, repeated.response.Settlement) {
		t.Fatalf("matching winner depended on current transport: code=%d err=%v", repeated.code, repeated.err)
	}
}

func TestInspectionSettlementCLITrackedRecoveryBeforeObsoleteExecutableProbe(t *testing.T) {
	for _, kind := range []string{"intent-only", "exact"} {
		t.Run(kind, func(t *testing.T) {
			f := prepareHistoricalObservationPhase(t, pueue.StateRunning)
			// Record an obsolete pair without removing the component owner's
			// actual installation or changing its cleanup authority.
			removed := filepath.Join(t.TempDir(), "removed")
			f.meta.SupervisorConfig.ClientExecutable = filepath.Join(removed, "pueue")
			f.meta.SupervisorConfig.DaemonExecutable = filepath.Join(removed, "pueued")
			op := inspectionSettlementOperation(t, f, time.Now(), true)
			recordInspectionSettlementLifetimeKind(t, op, kind)
			deps := f.dependencies()
			deps.InitialSupervisorExecutable = filepath.Join(removed, "pueue")
			var commands atomic.Int32
			deps.SupervisorOptions.Observer = func(pueue.CommandEvent) { commands.Add(1) }
			args := Arguments{Root: f.root, TaskID: op.Request().IdentityID(), SettlementKind: "inspection", SettlementPrepare: true}
			before := supervisorfixture.SnapshotStore(t, f.root)
			result := settleContext(t.Context(), args, deps)
			var fact *task.ControlFactError
			if result.code == 0 || !errors.As(result.err, &fact) || fact.ReasonCode != "automatic_recovery_available" || fact.NextAction != "run_supervisor_ensure_owner" {
				t.Fatalf("tracked recovery preflight: code=%d err=%v fact=%+v", result.code, result.err, fact)
			}
			if result.response.Settlement != nil || commands.Load() != 0 {
				t.Fatal("tracked recovery produced a human challenge or probed a supervisor executable")
			}
			if !reflect.DeepEqual(before, supervisorfixture.SnapshotStore(t, f.root)) {
				t.Fatal("tracked recovery preparation mutated root")
			}
		})
	}
}

func TestInspectionSettlementCLIAutomaticPathsBeforeMissingExecutableProbe(t *testing.T) {
	for _, completed := range []bool{true, false} {
		name := "expired-unstarted"
		reason := "automatic_recovery_available"
		if completed {
			name = "historical-terminal"
			reason = "maintenance_already_eligible"
		}
		t.Run(name, func(t *testing.T) {
			f := prepareHistoricalObservationPhase(t, pueue.StateRunning)
			start := time.Now().Add(-time.Hour)
			op := inspectionSettlementOperation(t, f, start, !completed)
			_, err := op.ClaimSubmissionContext(t.Context(), start.Add(time.Millisecond))
			requireSettlementTest(t, err)
			if completed {
				_, err = op.ClaimStart(start.Add(time.Second))
				requireSettlementTest(t, err)
				requireSettlementTest(t, op.RecordReceipt(42))
				requireSettlementTest(t, op.Complete(inspection.ResultEligible, []byte(`{"eligible":true}`), start.Add(2*time.Second)))
				requireSettlementTest(t, op.RecordWorkerSuccess(42))
			}
			deps := f.dependencies()
			deps.InitialSupervisorExecutable = filepath.Join(t.TempDir(), "removed", "pueue")
			args := Arguments{Root: f.root, TaskID: op.Request().IdentityID(), SettlementKind: "inspection", SettlementPrepare: true}
			before := supervisorfixture.SnapshotStore(t, f.root)
			result := settleContext(t.Context(), args, deps)
			var fact *task.ControlFactError
			if result.code == 0 || !errors.As(result.err, &fact) || fact.ReasonCode != reason || fact.NextAction != "run_supervisor_ensure_owner" {
				t.Fatalf("automatic preflight: code=%d err=%v fact=%+v", result.code, result.err, fact)
			}
			if !reflect.DeepEqual(before, supervisorfixture.SnapshotStore(t, f.root)) {
				t.Fatal("automatic preflight mutated root")
			}
		})
	}
}

func TestInspectionSettlementCLINotFoundNamesNamespace(t *testing.T) {
	f := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	result := settleContext(t.Context(), Arguments{Root: f.root, TaskID: strings.Repeat("f", 32), SettlementKind: "inspection", SettlementPrepare: true}, f.dependencies())
	var fact *task.ControlFactError
	if result.code == 0 || !errors.As(result.err, &fact) || fact.ResourceKind != "inspection" || fact.ResourceID != strings.Repeat("f", 32) {
		t.Fatalf("inspection not found: code=%d err=%v", result.code, result.err)
	}
}

func TestInspectionSettlementArgumentsChooseExplicitNamespace(t *testing.T) {
	args, err := ParseArguments([]string{"settle", waitFirstID, "--kind", "inspection", "--prepare", "--json"})
	requireSettlementTest(t, err)
	if args.SettlementKind != "inspection" || !args.SettlementPrepare {
		t.Fatalf("inspection namespace not preserved: %+v", args)
	}
	if _, err = ParseArguments([]string{"settle", waitFirstID, "--kind", "workspace", "--prepare"}); err == nil {
		t.Fatal("unimplemented settlement namespace accepted")
	}
}
