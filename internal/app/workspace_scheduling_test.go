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
	deps := Dependencies{ResourceChecker: fixtureResources, InitialSupervisorExecutable: supervisor.Binding().ClientExecutable, SupervisorOptions: options}
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
	deps := Dependencies{ResourceChecker: fixtureResources, SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { commands.Add(1) }}}
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
	deps := Dependencies{ResourceChecker: fixtureResources, SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { commands.Add(1) }}}
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
	deps := Dependencies{ResourceChecker: fixtureResources, RunnerExecutable: executablePath(t), InitialSupervisorExecutable: supervisor.Binding().ClientExecutable}
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
		ResourceChecker:  fixtureResources,
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

// The short-cutoff arithmetic is owned by the pure control-transaction test in
// internal/workspaces. This test proves the real stores are wired to that
// owner: a canceled observer still reaches the callback on a live, detached
// scheduling context bounded by the helper's original deadline.
func TestWorkspaceSchedulingReplayRetainsOriginalDeadlineAfterObserverCancellation(t *testing.T) {
	fixture := testsupport.NewRecoveryFixture(t)
	request := workspaceCleanupRecoveryRequest(t, fixture, workspace.OperationForget)
	operation := testsupport.OpenSelectionOperation(t, fixture, request)
	intent, err := fixture.Resource.ForgetIntent(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	purpose := workspaces.ForgetPurpose(intent)
	if _, err = fixture.Resource.AssociateOperation(t.Context(), purpose, request); err != nil {
		t.Fatal(err)
	}
	before := captureWorkspaceSchedulingIdentity(t, fixture, purpose, operation)
	subject, err := workspaces.ForgetSubject(intent)
	if err != nil {
		t.Fatal(err)
	}
	var commands, callbacks atomic.Int32
	deps := Dependencies{ResourceChecker: fixtureResources, SupervisorOptions: pueue.Options{ObservationTimeout: time.Hour, Observer: func(pueue.CommandEvent) { commands.Add(1) }}}
	observer, cancelObserver := context.WithCancel(t.Context())
	cancelObserver()
	barrier := errors.New("stop before submission")
	beforeSubmit := detachedOriginalDeadlineBarrier(t, request.OperationID, &callbacks, barrier)
	scheduled, err := workspaces.ScheduleOperation(observer, fixture.Store, fixture.Resource, workspaceRuntimeOptions(deps), workspaces.OperationSpec{Purpose: purpose, Kind: workspace.OperationForget, Subject: subject, Budget: time.Hour}, beforeSubmit)
	if scheduled == nil {
		t.Fatalf("replay lost its operation: %v", err)
	}
	defer func() {
		if closeErr := workspaces.CloseOperation(scheduled); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if !errors.Is(err, barrier) || callbacks.Load() != 1 || !errors.Is(observer.Err(), context.Canceled) {
		t.Fatalf("replay did not reach the submission barrier exactly once: err=%v callbacks=%d observer=%v", err, callbacks.Load(), observer.Err())
	}
	if after := captureWorkspaceSchedulingIdentity(t, fixture, purpose, scheduled); after != before {
		t.Fatalf("replay replaced its original request or purpose mapping: before=%+v after=%+v", before, after)
	}
	if _, err = scheduled.Receipt(t.Context()); !workspace.OperationAbsent(err) || commands.Load() != 0 {
		t.Fatalf("replay submitted before its barrier: commands=%d receipt err=%v", commands.Load(), err)
	}
}

func detachedOriginalDeadlineBarrier(t *testing.T, operationID string, callbacks *atomic.Int32, barrier error) func(context.Context, *workspace.Operation) error {
	t.Helper()
	return func(ctx context.Context, replayed *workspace.Operation) error {
		callbacks.Add(1)
		deadline, bounded := ctx.Deadline()
		if ctx.Err() != nil || !bounded || !deadline.Equal(replayed.Deadline()) || replayed.Request().OperationID != operationID {
			t.Errorf("replay did not run on its detached original-deadline context: err=%v deadline=%v bounded=%t operation=%s", ctx.Err(), deadline, bounded, replayed.Request().OperationID)
		}
		return barrier
	}
}

type workspaceSchedulingIdentity struct {
	request   string
	reference workspace.OperationReference
}

func captureWorkspaceSchedulingIdentity(t *testing.T, fixture *testsupport.RecoveryFixture, purpose string, operation *workspace.Operation) workspaceSchedulingIdentity {
	t.Helper()
	request, err := task.MarshalCanonical(operation.Request())
	if err != nil {
		t.Fatal(err)
	}
	reference, err := fixture.Resource.AssociatedOperation(t.Context(), purpose)
	if err != nil {
		t.Fatal(err)
	}
	return workspaceSchedulingIdentity{request: string(request), reference: reference}
}

// loadWorkspaceSchedulingIdentity reads the stored identity through a handle
// that is released before the caller schedules a replay on the same operation.
func loadWorkspaceSchedulingIdentity(t *testing.T, fixture *testsupport.RecoveryFixture, purpose string) workspaceSchedulingIdentity {
	t.Helper()
	original, err := workspace.LoadOperation(t.Context(), fixture.Store, lifetimeGuardianTestTaskID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := original.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	return captureWorkspaceSchedulingIdentity(t, fixture, purpose, original)
}

func TestWorkspaceScheduleOperationExpiredReplayOnlyClosesOriginalHelper(t *testing.T) {
	fixture := testsupport.NewRecoveryFixture(t)
	supervisor, _ := newTerminalEventInspectionSupervisor(t, fixture.Store)
	intent := workspace.ForgetIntent{
		SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID,
		OperationID: lifetimeGuardianTestTaskID, BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
	}
	if err := fixture.Resource.RecordForgetIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	seedWorkspaceForgetPurposeWithoutResourceReference(t, fixture, intent, supervisor.Binding())
	purpose := workspaces.ForgetPurpose(intent)
	subject, err := workspaces.ForgetSubject(intent)
	if err != nil {
		t.Fatal(err)
	}
	before := loadWorkspaceSchedulingIdentity(t, fixture, purpose)
	var commands, callbacks atomic.Int32
	sentinel := errors.New("expired replay reached submission")
	deps := Dependencies{
		ResourceChecker:  fixtureResources,
		RunnerExecutable: executablePath(t), InitialSupervisorExecutable: supervisor.Binding().ClientExecutable,
		SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { commands.Add(1) }},
	}
	beforeSubmit := func(context.Context, *workspace.Operation) error {
		callbacks.Add(1)
		return sentinel
	}
	for _, budget := range []time.Duration{time.Minute, 2 * time.Hour} {
		t.Run(budget.String(), func(t *testing.T) {
			observer, cancelObserver := context.WithCancel(t.Context())
			cancelObserver()
			scheduled, scheduleErr := workspaces.ScheduleOperation(observer, fixture.Store, fixture.Resource, workspaceRuntimeOptions(deps), workspaces.OperationSpec{Purpose: purpose, Kind: workspace.OperationForget, Subject: subject, Budget: budget}, beforeSubmit)
			if scheduled == nil {
				t.Fatalf("expired replay with budget %s lost its operation: %v", budget, scheduleErr)
			}
			t.Cleanup(func() {
				if closeErr := workspaces.CloseOperation(scheduled); closeErr != nil {
					t.Error(closeErr)
				}
			})
			verifyExpiredWorkspaceReplay(t, fixture, scheduled, purpose, intent.Deadline, before)
			if scheduleErr != nil || callbacks.Load() != 0 || commands.Load() != 0 {
				t.Fatalf("expired replay with budget %s did control work beyond closing: err=%v callbacks=%d commands=%d", budget, scheduleErr, callbacks.Load(), commands.Load())
			}
		})
	}
}

func verifyExpiredWorkspaceReplay(t *testing.T, fixture *testsupport.RecoveryFixture, scheduled *workspace.Operation, purpose, deadline string, before workspaceSchedulingIdentity) {
	t.Helper()
	if after := captureWorkspaceSchedulingIdentity(t, fixture, purpose, scheduled); after != before || scheduled.Request().Deadline != deadline {
		t.Fatalf("expired replay changed its request, mapping, or deadline: before=%+v after=%+v deadline=%s", before, after, scheduled.Request().Deadline)
	}
	fence, err := scheduled.ReadLifetimeUnlaunchedContext(t.Context())
	if err != nil || fence == nil || fence.Deadline != deadline {
		t.Fatalf("expired replay did not fence the original expiry: fence=%+v err=%v", fence, err)
	}
	if _, err = scheduled.SubmissionPermit(t.Context()); !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("expired replay reopened submission: %v", err)
	}
	if _, err = workspaces.OperationClosed(t.Context(), scheduled); err != nil {
		t.Fatalf("expired replay did not leave a closed-operation proof: %v", err)
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
	deps := Dependencies{ResourceChecker: fixtureResources, RunnerExecutable: executablePath(t), InitialSupervisorExecutable: supervisor.Binding().ClientExecutable}
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
