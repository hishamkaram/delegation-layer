package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	testsupportcore "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	"github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestWorkspaceSchedulingSurvivesObserverCancellationAfterAdd(t *testing.T) {
	fixture := testsupport.NewRecoveryFixture(t)
	supervisor, _ := newTerminalEventInspectionSupervisor(t, fixture.Store)
	request := workspaceCleanupRecoveryRequest(t, fixture, workspace.OperationForget)
	request.Supervisor = supervisor.Binding()
	request.WorkerExecutable = executablePath(t)
	digest, err := commonprovider.FingerprintExecutable(request.WorkerExecutable)
	if err != nil {
		t.Fatal(err)
	}
	request.WorkerSHA256 = digest
	operation := testsupport.OpenSelectionOperation(t, fixture, request)
	intent, err := fixture.Resource.ForgetIntent(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.Resource.AssociateOperation(t.Context(), workspaces.ForgetPurpose(intent), request); err != nil {
		t.Fatal(err)
	}
	observer, cancel := context.WithCancel(t.Context())
	defer cancel()
	var adds, enqueues atomic.Int32
	var receiptFault atomic.Bool
	options := pueue.Options{Environment: os.Environ(), Observer: workspaceAddCancellationObserver(t, operation, cancel, &adds, &enqueues, &receiptFault)}
	deps := Dependencies{InitialSupervisorExecutable: supervisor.Binding().ClientExecutable, SupervisorOptions: options}
	err = workspaces.ScheduleExistingOperation(observer, fixture.Store, operation, workspaceRuntimeOptions(deps))
	if err != nil || !errors.Is(observer.Err(), context.Canceled) || receiptFault.Load() {
		t.Fatalf("scheduling lost receipt-before-enqueue at observer cancellation: %v, observer=%v, receiptFault=%t", err, observer.Err(), receiptFault.Load())
	}
	if adds.Load() != 1 || enqueues.Load() != 1 || operation.Request().Deadline != request.Deadline {
		t.Fatalf("scheduling repeated or renewed the helper: add=%d enqueue=%d deadline=%s", adds.Load(), enqueues.Load(), operation.Request().Deadline)
	}
}

func workspaceAddCancellationObserver(t *testing.T, operation *workspace.Operation, cancel context.CancelFunc, adds, enqueues *atomic.Int32, receiptFault *atomic.Bool) func(pueue.CommandEvent) {
	t.Helper()
	return func(event pueue.CommandEvent) {
		if slices.Contains(event.Argv, "--help") {
			return
		}
		if event.Stage == "completed" && slices.Contains(event.Argv, "add") {
			adds.Add(1)
			cancel()
		}
		if event.Stage == "entry" && slices.Contains(event.Argv, "enqueue") {
			enqueues.Add(1)
			receipt, readErr := operation.Receipt(t.Context())
			if readErr != nil || receipt.NumericTaskID != 9 {
				receiptFault.Store(true)
			}
		}
	}
}

func executablePath(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWorkspaceSchedulingExpiredReplayOnlyClosesOriginalHelper(t *testing.T) {
	store, operation := testsupport.NewOperationDeadline(t, time.Now().Add(-time.Second))
	before, err := task.MarshalCanonical(operation.Request())
	if err != nil {
		t.Fatal(err)
	}
	var commands atomic.Int32
	deps := Dependencies{SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { commands.Add(1) }}}
	observer, cancel := context.WithCancel(t.Context())
	cancel()
	if err = workspaces.ScheduleExistingOperation(observer, store, operation, workspaceRuntimeOptions(deps)); err != nil {
		t.Fatal(err)
	}
	fence, err := operation.ReadLifetimeUnlaunchedContext(t.Context())
	if err != nil || fence == nil || commands.Load() != 0 {
		t.Fatalf("expired helper performed control work rather than recording its no-launch proof: fence=%+v commands=%d err=%v", fence, commands.Load(), err)
	}
	if _, err = operation.SubmissionPermit(t.Context()); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("expired replay reopened submission: %v", err)
	}
	after, err := task.MarshalCanonical(operation.Request())
	if err != nil || string(before) != string(after) {
		t.Fatalf("expired replay changed the original request: %v", err)
	}
}

func TestWorkspaceRunningHelperObservationDoesNotContactSupervisor(t *testing.T) {
	store, operation := testsupport.NewOperation(t)
	_, err := operation.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "pg:" + lifetimeGuardianTestTaskID, BootID: testsupportcore.InspectionRecoveryBootID(t), Deadline: operation.Deadline(),
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := operation.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := owner.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	var commands atomic.Int32
	deps := Dependencies{SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { commands.Add(1) }}}
	if err = workspaces.SubmitOperation(t.Context(), store, operation, workspaceRuntimeOptions(deps)); err != nil {
		t.Fatal(err)
	}
	if err = workspaces.ScheduleExistingOperation(t.Context(), store, operation, workspaceRuntimeOptions(deps)); err != nil {
		t.Fatal(err)
	}
	if commands.Load() != 0 {
		t.Fatalf("running helper observation contacted supervisor %d times", commands.Load())
	}
	if _, err = operation.ReadLifetimeWorkerWaitContext(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("observation fabricated a live helper's completion: %v", err)
	}
}

func TestWorkspaceVerificationBeforeAuthorizationIsNotIndexed(t *testing.T) {
	fixture := testsupport.NewRecoveryFixture(t)
	supervisor, _ := newTerminalEventInspectionSupervisor(t, fixture.Store)
	subject := workspace.Subject{Kind: workspace.SubjectAdmission, ID: lifetimeGuardianTestTaskID, SHA256: task.ComputeSHA256([]byte("verification authorization"))}
	deps := Dependencies{RunnerExecutable: executablePath(t), InitialSupervisorExecutable: supervisor.Binding().ClientExecutable}
	operation, err := workspaces.ScheduleOperation(t.Context(), fixture.Store, fixture.Resource, workspaceRuntimeOptions(deps), workspaces.OperationSpec{Purpose: "verify-" + lifetimeGuardianTestTaskID + ".json", Kind: workspace.OperationVerify, Subject: subject, Budget: time.Minute, Arguments: []string{"/fixture/verify"}}, func(context.Context, *workspace.Operation) error { return context.DeadlineExceeded })
	defer func() {
		if closeErr := workspaces.CloseOperation(operation); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if operation == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fixture did not reach the pre-authorization boundary: operation=%v err=%v", operation, err)
	}
	ids, err := fixture.Resource.OperationIDs(t.Context())
	if err != nil || len(ids) != 0 {
		t.Fatalf("unauthorized verification became an unfinished resource use: %v, %v", ids, err)
	}
	if _, err = workspaces.ForgetInventory(t.Context(), fixture.Store, fixture.Resource, time.Minute); err != nil {
		t.Fatalf("unauthorized verification blocked forgetting: %v", err)
	}
}

func TestWorkspaceExpiredFirstForgetCreatesOnlyMetadataAndFence(t *testing.T) {
	fixture := testsupport.NewRecoveryFixture(t)
	supervisor, _ := newTerminalEventInspectionSupervisor(t, fixture.Store)
	intent := workspace.ForgetIntent{
		SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID,
		OperationID: lifetimeGuardianTestTaskID, BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
	}
	if err := fixture.Resource.RecordForgetIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	var commands atomic.Int32
	deps := Dependencies{
		RunnerExecutable: executablePath(t), InitialSupervisorExecutable: supervisor.Binding().ClientExecutable,
		SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { commands.Add(1) }},
	}
	id, handled, err := workspaces.ReconcileForgetOperation(t.Context(), fixture.Store, fixture.Resource, workspaceRuntimeOptions(deps), false, 0)
	if !handled || id == "" || !workspaces.PendingError(err) || commands.Load() != 0 {
		t.Fatalf("expired metadata recovery contacted the supervisor: id=%s commands=%d err=%v", id, commands.Load(), err)
	}
	operation, err := workspace.LoadOperation(t.Context(), fixture.Store, id)
	if err != nil {
		t.Fatal(err)
	}
	fence, fenceErr := operation.ReadLifetimeUnlaunchedContext(t.Context())
	err = errors.Join(fenceErr, operation.Close())
	if err != nil || fence.Deadline != intent.Deadline {
		t.Fatalf("metadata-only recovery did not fence the original deadline: %+v, %v", fence, err)
	}
}

func TestWorkspaceSchedulingReplayRetainsOriginalDeadlineAndControlCutoff(t *testing.T) {
	for _, timeout := range []time.Duration{10 * time.Millisecond, time.Hour} {
		t.Run(timeout.String(), func(t *testing.T) { verifyWorkspaceSchedulingReplay(t, timeout) })
	}
}

func verifyWorkspaceSchedulingReplay(t *testing.T, timeout time.Duration) {
	t.Helper()
	fixture := testsupport.NewRecoveryFixture(t)
	request := workspaceCleanupRecoveryRequest(t, fixture, workspace.OperationForget)
	testsupport.OpenSelectionOperation(t, fixture, request)
	intent, err := fixture.Resource.ForgetIntent(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.Resource.AssociateOperation(t.Context(), workspaces.ForgetPurpose(intent), request); err != nil {
		t.Fatal(err)
	}
	options := pueue.Options{ObservationTimeout: timeout}
	started := time.Now()
	barrier := errors.New("stop before submission")
	before := func(ctx context.Context, operation *workspace.Operation) error {
		deadline, bounded := ctx.Deadline()
		if !bounded || deadline.After(operation.Deadline()) || deadline.After(time.Now().Add(pueue.PrivateControlTimeout(options))) || deadline.Before(started) {
			t.Errorf("replay did not retain the original bounded scheduling context: %v", deadline)
		}
		return barrier
	}
	subject, err := workspaces.ForgetSubject(intent)
	if err != nil {
		t.Fatal(err)
	}
	scheduled, err := workspaces.ScheduleOperation(t.Context(), fixture.Store, fixture.Resource, workspaceRuntimeOptions(Dependencies{SupervisorOptions: options}), workspaces.OperationSpec{Purpose: workspaces.ForgetPurpose(intent), Kind: workspace.OperationForget, Subject: subject, Budget: time.Hour}, before)
	if !errors.Is(err, barrier) || scheduled == nil || scheduled.Request().Deadline != request.Deadline {
		t.Fatalf("replay replaced its original request: operation=%v err=%v", scheduled, err)
	}
	if err = workspaces.CloseOperation(scheduled); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceExplicitForgetRecoversExpiredUncreatedFirstHelper(t *testing.T) {
	for _, associated := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-request", true: "before-resource-reference"}[associated], func(t *testing.T) { verifyExpiredFirstWorkspaceForgetRecovery(t, associated) })
	}
}

func verifyExpiredFirstWorkspaceForgetRecovery(t *testing.T, associated bool) {
	t.Helper()
	fixture := testsupport.NewRecoveryFixture(t)
	supervisor, _ := newTerminalEventInspectionSupervisor(t, fixture.Store)
	intent := workspace.ForgetIntent{
		SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID,
		OperationID: lifetimeGuardianTestTaskID, BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
	}
	if err := fixture.Resource.RecordForgetIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	if associated {
		seedWorkspaceForgetPurposeWithoutResourceReference(t, fixture, intent, supervisor.Binding())
	}
	deps := Dependencies{RunnerExecutable: executablePath(t), InitialSupervisorExecutable: supervisor.Binding().ClientExecutable}
	operationID, handled, err := workspaces.ReconcileForgetOperation(t.Context(), fixture.Store, fixture.Resource, workspaceRuntimeOptions(deps), true, time.Minute)
	if !handled || !workspaces.PendingError(err) || operationID == "" {
		t.Fatalf("explicit retry could not advance an expired uncreated first helper: id=%s handled=%t err=%v", operationID, handled, err)
	}
	first, err := fixture.Resource.AssociatedOperation(t.Context(), workspaces.ForgetPurpose(intent))
	if err != nil || first.OperationID == operationID {
		t.Fatalf("explicit retry did not retire the original first attempt: %+v err=%v", first, err)
	}
	operation, err := workspace.LoadOperation(t.Context(), fixture.Store, first.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	fence, err := operation.ReadLifetimeUnlaunchedContext(t.Context())
	if err != nil || fence.Deadline != intent.Deadline || operation.Request().Deadline != intent.Deadline {
		t.Fatalf("explicit retry renewed or launched the expired original: fence=%+v err=%v", fence, err)
	}
}

func seedWorkspaceForgetPurposeWithoutResourceReference(t *testing.T, fixture *testsupport.RecoveryFixture, intent workspace.ForgetIntent, binding task.SupervisorRef) {
	t.Helper()
	subject, err := workspaces.ForgetSubject(intent)
	if err != nil {
		t.Fatal(err)
	}
	request := testsupport.RecoveryOperationRequest(t, fixture, lifetimeGuardianTestTaskID, workspace.OperationForget, subject)
	request.Deadline, request.Supervisor = intent.Deadline, binding
	operation, err := workspace.OpenOperation(t.Context(), fixture.Store, request)
	if err != nil {
		t.Fatal(err)
	}
	_, associateErr := fixture.Resource.AssociateOperation(t.Context(), workspaces.ForgetPurpose(intent), request)
	if err = errors.Join(associateErr, operation.Close()); err != nil {
		t.Fatal(err)
	}
	ids, err := fixture.Resource.OperationIDs(t.Context())
	if err != nil || len(ids) != 0 {
		t.Fatalf("fixture advanced past the interrupted resource-reference write: %v, %v", ids, err)
	}
}
