package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

// These consumers once continued on any error carrying ErrPending. The use
// path reaches the production recovery of an unfinished admission helper
// without lifetime intent, whose deferred owner release fails once.

// pendingAdmissionHelperID names the admission's unexpired provision helper.
var pendingAdmissionHelperID = strings.Repeat("6", 32)

// expiredVerificationID names a different, expired verification helper.
var expiredVerificationID = strings.Repeat("7", 32)

// newPendingAdmissionUse reserves the workspace for an admission whose
// provision helper is persisted, unexpired and has no lifetime intent.
func newPendingAdmissionUse(t *testing.T) *workspacefixture.RecoveryFixture {
	t.Helper()
	fixture, reservation := newWorkspaceInspectionReservation(t)
	workspacefixture.OpenSelectionOperation(t, fixture, pendingAdmissionHelperRequest(t, fixture, reservation))
	return fixture
}

func pendingAdmissionHelperRequest(t *testing.T, fixture *workspacefixture.RecoveryFixture, reservation workspace.Reservation) workspace.OperationRequest {
	t.Helper()
	return workspacefixture.RecoveryOperationRequest(t, fixture, pendingAdmissionHelperID, workspace.OperationProvision, reservation.Subject)
}

// persistPendingAdmissionUse persists newPendingAdmissionUse's records in a
// parent fixture under a transient use owner; the child takes the use owner.
func persistPendingAdmissionUse(t *testing.T, fixture *workspacefixture.RecoveryFixture) {
	t.Helper()
	owner, err := fixture.Resource.TryAcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request := fixture.Resource.Request()
	reservation, err := fixture.Resource.Reserve(t.Context(), workspace.Subject{Kind: workspace.SubjectAdmission, ID: request.TaskID, SHA256: request.RequestSHA256})
	if err = errors.Join(err, owner.Release()); err != nil {
		t.Fatal(err)
	}
	workspacefixture.PersistSelectionOperation(t, fixture, pendingAdmissionHelperRequest(t, fixture, reservation))
}

func persistPendingAdmissionWithExpiredVerification(t *testing.T, fixture *workspacefixture.RecoveryFixture) {
	t.Helper()
	persistPendingAdmissionUse(t, fixture)
	workspacefixture.PersistSelectionOperation(t, fixture, expiredVerificationRequest(t, fixture))
}

// pendingAdmissionUseChild runs the calling test's fault half in a child
// process holding the workspace use owner, or returns nil in the parent.
func pendingAdmissionUseChild(t *testing.T, persist func(*testing.T, *workspacefixture.RecoveryFixture)) *workspacefixture.RecoveryFixture {
	t.Helper()
	fixture := workspacefixture.OwnerReleaseRecoveryFixture(t, persist, pendingAdmissionHelperID)
	if fixture != nil {
		workspacefixture.RecoveryUseOwner(t, fixture)
	}
	return fixture
}

// expiredVerificationRequest is a different, expired verification helper
// whose closure would seal its unlaunched fence.
func expiredVerificationRequest(t *testing.T, fixture *workspacefixture.RecoveryFixture) workspace.OperationRequest {
	t.Helper()
	request := workspacefixture.RecoveryOperationRequest(t, fixture, expiredVerificationID, workspace.OperationVerify, workspace.Subject{Kind: workspace.SubjectOperation, ID: expiredVerificationID, SHA256: task.ComputeSHA256([]byte("verification"))})
	request.Deadline = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	request.Arguments = []string{"/usr/bin/true"}
	return request
}

// watchDuration is the public status action's watch and deadline.
const watchDuration = 500 * time.Millisecond

// watchOutcome keeps the status action's own error beside its public
// projection, which drops the error of a pending exit 3.
type watchOutcome struct {
	actionErr error
	result    commandResult
}

// watchStatus runs the public status action and its projection over the
// fixture's production store, as workspaceCommand does under its use owner.
func watchStatus(t *testing.T, fixture *workspacefixture.RecoveryFixture, operationID string) watchOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), watchDuration)
	defer cancel()
	return workspaceWatch(ctx, fixture, operationID)
}

func workspaceWatch(ctx context.Context, fixture *workspacefixture.RecoveryFixture, operationID string) watchOutcome {
	a := Arguments{WorkspaceAction: "status", Watch: watchDuration, WorkspaceOperationID: operationID}
	ack, err := runWorkspaceCommandAction(ctx, a, fixture.Store, fixture.Resource, Dependencies{}, time.Minute)
	return watchOutcome{actionErr: err, result: workspaceCommandActionResponse(ctx, fixture.Store, fixture.Resource, a.WorkspaceAction, false, ack, newResponse("workspace"), err)}
}

// assertPendingWorkspaceVisible requires the response to describe the
// still-reserved workspace and the selected operation. That description is
// the resource's own projection; it is independent of the observer's exit.
func assertPendingWorkspaceVisible(t *testing.T, selector string, result commandResult) {
	t.Helper()
	described := result.response.Workspace
	if described == nil || described.State != "busy" || described.NextAction != "watch_workspace" || described.OperationID != selector {
		t.Fatalf("watch with selector %q described %+v; want the busy workspace to watch", selector, described)
	}
}

// watchSelectors are the nil and exact operation selectors of a watch.
var watchSelectors = map[string]string{"no selector": "", "exact selector": pendingAdmissionHelperID}

func TestWorkspaceWatchReturnsUseFaultJoinedWithPending(t *testing.T) {
	for name, selector := range watchSelectors {
		t.Run(name, func(t *testing.T) {
			fixture := pendingAdmissionUseChild(t, persistPendingAdmissionUse)
			if fixture == nil {
				return
			}
			fault := injectOperationOwnerUnlockFault(t, fixture.Store, pendingAdmissionHelperID)
			observed := watchStatus(t, fixture, selector)
			result := observed.result
			if workspacescore.PendingError(observed.actionErr) {
				t.Fatalf("watch with selector %q classified its use fault as pending: %v", selector, observed.actionErr)
			}
			if !fault.fired || result.code != 1 || !errors.Is(result.err, errOperationOwnerUnlock) || !errors.Is(result.err, workspace.ErrPending) {
				t.Fatalf("watch with selector %q = %+v; want exit 1 with the whole use fault", selector, result)
			}
			if result.response.Workspace == nil || result.response.Workspace.OperationID != selector {
				t.Fatalf("watch with selector %q lost its operation ID: %+v", selector, result.response.Workspace)
			}
		})
	}
}

// The bubble's clock advances only while every goroutine is durably blocked,
// so record I/O consumes neither the watch nor the response-read budget. The
// exact elapsed time proves the pending watch returns at its virtual deadline;
// it is not a native latency bound.
func TestWorkspaceWatchPendingOnlyExpiresAsPending(t *testing.T) {
	for name, selector := range watchSelectors {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newPendingAdmissionUse(t)
				started := time.Now()
				observed := watchStatus(t, fixture, selector)
				if !errors.Is(observed.actionErr, context.DeadlineExceeded) || !workspacescore.PendingError(observed.actionErr) {
					t.Fatalf("expired watch with selector %q = %v; want its own deadline classified as pending", selector, observed.actionErr)
				}
				if observed.result.code != 3 || observed.result.err != nil {
					t.Fatalf("pending watch with selector %q = %+v; want exit 3", selector, observed.result)
				}
				assertPendingWorkspaceVisible(t, selector, observed.result)
				if elapsed := time.Since(started); elapsed != watchDuration {
					t.Fatalf("pending watch with selector %q returned after %v, want exactly its %v watch", selector, elapsed, watchDuration)
				}
			})
		})
	}
}

// canceledPollDelay cancels after the first poll interval has elapsed, so
// the watcher has completed a poll and is durably blocked in its next wait.
const canceledPollDelay = 150 * time.Millisecond

// Canceling only the observer is a fault of that observer: its pending
// marker stays joined with the cancellation, but the whole error is not
// pending. The workspace itself is still busy and is described as such.
func TestWorkspaceWatchCancellationDuringPollIsNotPending(t *testing.T) {
	for name, selector := range watchSelectors {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newPendingAdmissionUse(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				started := time.Now()
				outcomes := make(chan watchOutcome, 1)
				go func() { outcomes <- workspaceWatch(ctx, fixture, selector) }()
				time.Sleep(canceledPollDelay)
				cancel()
				observed := <-outcomes
				if elapsed := time.Since(started); elapsed != canceledPollDelay {
					t.Fatalf("canceled watch with selector %q returned after %v, want exactly its %v cancellation", selector, elapsed, canceledPollDelay)
				}
				assertCanceledWatch(t, selector, observed)
			})
		})
	}
}

func assertCanceledWatch(t *testing.T, selector string, observed watchOutcome) {
	t.Helper()
	for _, err := range []error{observed.actionErr, observed.result.err} {
		if !errors.Is(err, workspace.ErrPending) || !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || workspacescore.PendingError(err) {
			t.Fatalf("canceled watch with selector %q = %v; want cancellation retained and not pending", selector, err)
		}
	}
	if observed.result.code != 1 || observed.result.response.Error == "" {
		t.Fatalf("canceled watch with selector %q = code %d, error %q; want exit 1 with the reported cause", selector, observed.result.code, observed.result.response.Error)
	}
	assertPendingWorkspaceVisible(t, selector, observed.result)
}

// observeExpiredVerification observes the workspace selecting the expired
// verification helper and reads that helper's unlaunched fence.
func observeExpiredVerification(t *testing.T, fixture *workspacefixture.RecoveryFixture) (observeErr, fenceErr error) {
	t.Helper()
	observeErr = observeManagedWorkspace(t.Context(), fixture.Store, fixture.Resource, Dependencies{}, expiredVerificationID)
	selected, err := workspace.LoadOperation(t.Context(), fixture.Store, expiredVerificationID)
	if err != nil {
		t.Fatal(err)
	}
	_, fenceErr = selected.ReadLifetimeUnlaunchedContext(t.Context())
	if err = selected.Close(); err != nil {
		t.Fatal(err)
	}
	return observeErr, fenceErr
}

// TestWorkspaceObserveStopsBeforeSelectedOperationOnUseFault binds the
// no-effect claim to observeManagedWorkspace only: a final response may
// still describe the selected operation independently.
func TestWorkspaceObserveStopsBeforeSelectedOperationOnUseFault(t *testing.T) {
	fixture := pendingAdmissionUseChild(t, persistPendingAdmissionWithExpiredVerification)
	if fixture == nil {
		return
	}
	fault := injectOperationOwnerUnlockFault(t, fixture.Store, pendingAdmissionHelperID)
	err, fenceErr := observeExpiredVerification(t, fixture)
	if !fault.fired || !errors.Is(err, errOperationOwnerUnlock) || workspacescore.PendingError(err) || !workspace.OperationAbsent(fenceErr) {
		t.Fatalf("faulted observation = %v, selected fence %v; want the use fault before the selected operation", err, fenceErr)
	}
}

func TestWorkspaceObservePendingOnlyReachesSelectedOperation(t *testing.T) {
	fixture := newPendingAdmissionUse(t)
	workspacefixture.OpenSelectionOperation(t, fixture, expiredVerificationRequest(t, fixture))
	err, fenceErr := observeExpiredVerification(t, fixture)
	if !errors.Is(err, workspace.ErrPending) || !errors.Is(err, task.ErrLifetimeUnlaunched) || fenceErr != nil {
		t.Fatalf("pending observation = %v, selected fence %v; want the selected outcome", err, fenceErr)
	}
}

var errFixtureSubmissionRefused = errors.New("fixture supervisor refused submission")

// armedOwnerUnlockFault fails the first lifetime-owner unlock of any
// workspace operation once armed.
type armedOwnerUnlockFault struct {
	taskdir.BaseFaultInjector
	armed, fired bool
}

func (f *armedOwnerUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (f *armedOwnerUnlockFault) OnLockParentDirBarrier(string) error { return nil }
func (f *armedOwnerUnlockFault) OnFlock(path string, operation int) error {
	separator := string(filepath.Separator)
	if f.armed && !f.fired && operation == unix.LOCK_UN && strings.Contains(path, separator+"workspace-operations"+separator) && strings.HasSuffix(path, separator+".lifetime-owner.lock") {
		f.fired = true
		return errOperationOwnerUnlock
	}
	return nil
}

// refusingSubmissionRuntime refuses submission after the provision request is
// durable and arms the release fault for the admission's abort recovery.
type refusingSubmissionRuntime struct {
	checkpointTestRuntime
	fault *armedOwnerUnlockFault
}

func (r refusingSubmissionRuntime) OpenSaved(context.Context, string, task.SupervisorRef) (*pueue.Client, error) {
	r.fault.armed = true
	return nil, errFixtureSubmissionRefused
}

// provisionDispatch is one real dispatch of an isolated workspace over the
// fixture's production stores.
type provisionDispatch struct {
	runtime workspacescore.OperationRuntime
	// fault, when non-nil, is installed on every store the dispatch opens.
	fault taskdir.FaultInjector
	// budget is the requested workspace budget, which sets the helper deadline.
	budget string
	// taskID, when non-empty, is the caller's explicit task ID.
	taskID string
}

func (d provisionDispatch) run(fixture checkpointRuntimeFixture) tasklifecycle.DispatchResult {
	options := dispatchOptions(fixture.deps, false)
	options.Workspace = fixture.options
	options.Workspace.Runtime = d.runtime
	options.OpenStore = func(root string, create bool) (*taskdir.Store, error) {
		store, err := openStore(root, fixture.deps.storeDependencies(), create)
		if err == nil && d.fault != nil {
			store.SetFaultInjector(d.fault)
		}
		return store, err
	}
	return tasklifecycle.Dispatch(dispatchRequest(Arguments{
		Root: fixture.lineage.store.Root, Cwd: fixture.lineage.main, TaskID: d.taskID, Provider: "codex:exec", Isolation: "git-worktree", WorkspaceBudget: d.budget,
		BriefBytes: []byte("bounded task"), Config: task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m"},
	}), options)
}

// dispatchRefusedProvision dispatches an isolated workspace whose provision
// helper is persisted but never submitted, so Finish recovers the admission.
func dispatchRefusedProvision(t *testing.T, fixture checkpointRuntimeFixture, fault *armedOwnerUnlockFault, hooked bool) tasklifecycle.DispatchResult {
	t.Helper()
	var storeFault taskdir.FaultInjector
	if hooked {
		storeFault = fault
	}
	observed := provisionDispatch{runtime: refusingSubmissionRuntime{checkpointTestRuntime: fixture.runtime, fault: fault}, fault: storeFault, budget: "1m"}.run(fixture)
	if observed.Workspace == nil || observed.Phase != tasklifecycle.DispatchWorkspace || !errors.Is(observed.Failure, errFixtureSubmissionRefused) {
		t.Fatalf("refused provision dispatch = %+v", observed)
	}
	return observed
}

type pendingProvisionRuntime struct{ checkpointTestRuntime }

func (pendingProvisionRuntime) OpenSaved(context.Context, string, task.SupervisorRef) (*pueue.Client, error) {
	return nil, pueue.ErrInFlight
}

func dispatchPendingProvision(t *testing.T) tasklifecycle.DispatchResult {
	t.Helper()
	fixture := newCheckpointRuntimeFixture(t)
	return provisionDispatch{runtime: pendingProvisionRuntime{checkpointTestRuntime: fixture.runtime}, budget: "1m"}.run(fixture)
}

func assertPendingProvisionState(t *testing.T, observed tasklifecycle.DispatchResult) {
	t.Helper()
	if observed.Workspace == nil || observed.Phase != tasklifecycle.DispatchWorkspace || observed.RecordPresence() != tasklifecycle.RecordAbsent || !workspacescore.PendingError(observed.Failure) {
		t.Fatalf("pending provision dispatch = %+v", observed)
	}
	if observed.Workspace.OperationID == "" || observed.Workspace.Description.State != "busy" || observed.Workspace.Description.Path != "" {
		t.Fatalf("pending provision description = %+v", observed.Workspace)
	}
}

// dispatchWire projects a dispatch as the public dispatch command does and
// decodes the JSON response that command writes.
func dispatchWire(t *testing.T, observed tasklifecycle.DispatchResult) (commandResult, Response) {
	t.Helper()
	result := dispatchCommandResult(observed)
	var output bytes.Buffer
	if err := writeJSONResponse(&output, "dispatch", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return result, decoded
}

// dispatchGuidance is the public verdict of a failed managed dispatch.
type dispatchGuidance struct {
	exit       int
	taskRecord string
	code       string
	nextAction string
	// watch requires the workspace status watch as the recovery command;
	// otherwise the response must carry no recovery command.
	watch bool
}

var (
	provisionPendingGuidance = dispatchGuidance{exit: 3, taskRecord: "not_created", code: "workspace_provision_pending", nextAction: "watch_workspace", watch: true}
	unknownRecordGuidance    = dispatchGuidance{exit: 2, taskRecord: "unknown", code: "workspace_busy", nextAction: "stop_and_report"}
	preparationFaultGuidance = dispatchGuidance{exit: 2, taskRecord: "not_created", code: "workspace_preparation_failed", nextAction: "stop_and_report"}
)

// requireDispatchGuidance serializes the dispatch and requires want in its
// exit and decoded JSON, which still names the observed helper. It returns
// the projection for fault-identity checks of the command error.
func requireDispatchGuidance(t *testing.T, label string, observed tasklifecycle.DispatchResult, want dispatchGuidance) commandResult {
	t.Helper()
	result, decoded := dispatchWire(t, observed)
	recovery := ""
	if want.watch {
		recovery = workspaceStatusCommand(observed.Root, observed.Workspace.Description.ID)
	}
	failure := decoded.Failure
	if result.code != want.exit || decoded.TaskRecord != want.taskRecord || failure == nil || failure.Code != want.code || failure.Stage != "prepare-workspace" || failure.NextAction != want.nextAction || failure.RecoveryCommand != recovery || decoded.Error != dispatchFailureMessage(want.code) {
		t.Fatalf("%s: exit %d, wire response %+v (failure %+v); want %+v", label, result.code, decoded, failure, want)
	}
	if decoded.Workspace == nil || decoded.Workspace.OperationID != observed.Workspace.OperationID || decoded.Workspace.State != "busy" {
		t.Fatalf("%s: wire workspace %+v; want the busy workspace of helper %q", label, decoded.Workspace, observed.Workspace.OperationID)
	}
	return result
}

// The scheduling controls change one public fact of a real pending dispatch.
func assertPendingProvisionProjectionControls(t *testing.T, observed tasklifecycle.DispatchResult) {
	t.Helper()
	t.Run("unknown record", func(t *testing.T) {
		candidate := observed
		candidate.Record = tasklifecycle.RecordUnknown
		result := requireDispatchGuidance(t, "unknown record", candidate, unknownRecordGuidance)
		if !errors.Is(result.err, pueue.ErrInFlight) || !workspacescore.PendingError(result.err) {
			t.Fatalf("unknown record command error = %v; want the pending scheduling cause", result.err)
		}
	})
	t.Run("concrete fault", func(t *testing.T) {
		candidate := observed
		candidate.Failure = errors.Join(candidate.Failure, task.ErrEvidenceFault)
		result := requireDispatchGuidance(t, "concrete fault", candidate, preparationFaultGuidance)
		if !errors.Is(result.err, task.ErrEvidenceFault) || !errors.Is(result.err, pueue.ErrInFlight) || workspacescore.PendingError(result.err) {
			t.Fatalf("concrete fault command error = %v; want the whole joined fault", result.err)
		}
	})
}

// This admission stops while scheduling, before any helper observation.
func TestManagedDispatchProvisionPendingSurvivesPublicProjection(t *testing.T) {
	observed := dispatchPendingProvision(t)
	assertPendingProvisionState(t, observed)
	requireDispatchGuidance(t, "pending scheduling", observed, provisionPendingGuidance)
	assertPendingProvisionProjectionControls(t, observed)
}

// observedHelperBudget is the scheduled helper's budget. It outlasts the
// admission observer's private workspace.DefaultBudget and the barrier hold,
// so only the observer's own deadline expires.
const observedHelperBudget = "1h"

// observerExpiryMargin keeps a barrier holding past the admission observer's
// budget, so that observer's deadline ends its wait before the hold does.
const observerExpiryMargin = time.Second

// explicitDispatchTaskID is a caller-chosen task ID; a failed admission does
// not prove its record absent.
var explicitDispatchTaskID = strings.Repeat("8", 32)

// scheduledHelperJournal names the provision helper of a workspace-operations
// journal lock path, or returns "" for any other lock.
func scheduledHelperJournal(path, lock string) string {
	directory := filepath.Dir(path)
	if filepath.Base(path) != lock || filepath.Base(filepath.Dir(directory)) != "workspace-operations" {
		return ""
	}
	return filepath.Base(directory)
}

// observationLoad counts opens of the one helper's journal lock. Its first
// open creates the helper inside Schedule, after which the supervisor add has
// submitted it; its second is the admission observer's load after Schedule,
// Close and the post-Schedule context check. Finish opens it again later.
type observationLoad struct {
	t        *testing.T
	adds     *atomic.Int32
	helperID string
	opens    int
}

// opened records one lock open and reports whether it is the observer's load.
func (l *observationLoad) opened(path string) bool {
	id := scheduledHelperJournal(path, ".operation.lock")
	if id == "" {
		return false
	}
	if l.helperID == "" {
		l.helperID = id
	}
	if id != l.helperID {
		l.t.Errorf("dispatch opened a second helper %q beside %q", id, l.helperID)
		return false
	}
	l.opens++
	if l.opens != 2 {
		return false
	}
	if adds := l.adds.Load(); adds != 1 {
		l.t.Errorf("observer load of helper %q followed %d supervisor adds, want the one scheduling add", id, adds)
	}
	return true
}

func (l *observationLoad) requireObserved(t *testing.T, observed tasklifecycle.DispatchResult) {
	t.Helper()
	if l.opens < 2 || l.helperID == "" || observed.Workspace == nil || observed.Workspace.OperationID != l.helperID {
		t.Fatalf("helper %q opened %d times, dispatch workspace %+v; want the scheduled helper observed", l.helperID, l.opens, observed.Workspace)
	}
	if adds := l.adds.Load(); adds != 1 {
		t.Fatalf("supervisor adds = %d, want the one scheduling add", adds)
	}
}

// observationDeadlineBarrier holds the helper journal lock from a second
// descriptor once the admission observer loads it. The holder sleeps on a
// bubble timer past the observer's budget, so the observer's lock wait ends at
// its own deadline; Finish then waits for the release on virtual time.
type observationDeadlineBarrier struct {
	taskdir.BaseFaultInjector
	load     observationLoad
	holding  bool
	released chan struct{}
}

func newObservationDeadlineBarrier(t *testing.T, fixture checkpointRuntimeFixture) *observationDeadlineBarrier {
	return &observationDeadlineBarrier{load: observationLoad{t: t, adds: fixture.adds}, released: make(chan struct{})}
}

func (b *observationDeadlineBarrier) OnLockParentDirBarrier(string) error { return nil }
func (b *observationDeadlineBarrier) OnFlock(string, int) error           { return nil }

func (b *observationDeadlineBarrier) OnLockFileBarrier(path string) error {
	if !b.load.opened(path) {
		return nil
	}
	held, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	if err = unix.Flock(held, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.Join(err, unix.Close(held))
	}
	b.holding = true
	go func() {
		defer close(b.released)
		time.Sleep(workspace.DefaultBudget + observerExpiryMargin)
		if closeErr := unix.Close(held); closeErr != nil {
			b.load.t.Error(closeErr)
		}
	}()
	return nil
}

// requireHeldDeadline requires that the observer's load was held and that its
// own deadline, not a registration read, ended the scheduled admission.
func (b *observationDeadlineBarrier) requireHeldDeadline(t *testing.T, observed tasklifecycle.DispatchResult) {
	t.Helper()
	b.load.requireObserved(t, observed)
	if !b.holding {
		t.Fatal("barrier never held the observer's helper load")
	}
	<-b.released
	failure := observed.Failure
	if observed.Phase != tasklifecycle.DispatchWorkspace || !errors.Is(failure, task.ErrLockBusy) || !errors.Is(failure, context.DeadlineExceeded) || !errors.Is(failure, workspace.ErrPending) || !workspacescore.PendingError(failure) {
		t.Fatalf("held observation dispatch = %v in phase %v; want the observer's lock wait expired as pending", failure, observed.Phase)
	}
	finished := observed.Workspace
	if finished.AbortError != nil || finished.DescriptionError != nil || finished.CloseError != nil || finished.Description.State != "busy" || finished.Description.Path != "" {
		t.Fatalf("held observation finish = %+v; want the provisioning workspace described without faults", finished)
	}
	requireHelperDeadlineAhead(t, observed)
}

// requireHelperDeadlineAhead reopens the helper journal after the dispatch
// closed its stores: virtual time spent only the observer's budget.
func requireHelperDeadlineAhead(t *testing.T, observed tasklifecycle.DispatchResult) {
	t.Helper()
	store, err := taskdir.OpenStore(observed.Root)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := workspace.LoadOperation(t.Context(), store, observed.Workspace.OperationID)
	if err != nil {
		t.Fatal(errors.Join(err, store.Close()))
	}
	deadline := operation.Deadline()
	if err = errors.Join(operation.Close(), store.Close()); err != nil {
		t.Fatal(err)
	}
	if !time.Now().Before(deadline) {
		t.Fatalf("helper deadline %v passed by %v; want only the observer budget spent", deadline, time.Now())
	}
}

// Inside the bubble, PrepareDispatch's private workspace.DefaultBudget runs
// on virtual time, which advances only while every goroutine is durably
// blocked: real Schedule, Close, record and Git I/O spend none of it.
func dispatchHeldObservation(t *testing.T, fixture checkpointRuntimeFixture, taskID string) tasklifecycle.DispatchResult {
	t.Helper()
	barrier := newObservationDeadlineBarrier(t, fixture)
	observed := provisionDispatch{runtime: fixture.runtime, fault: barrier, budget: observedHelperBudget, taskID: taskID}.run(fixture)
	barrier.requireHeldDeadline(t, observed)
	return observed
}

func TestManagedDispatchObservationDeadlineReachesPublicJSON(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	synctest.Test(t, func(t *testing.T) {
		observed := dispatchHeldObservation(t, fixture, "")
		if observed.RecordPresence() != tasklifecycle.RecordAbsent {
			t.Fatalf("held observation record = %v, want absent", observed.Record)
		}
		result := requireDispatchGuidance(t, "observer deadline", observed, provisionPendingGuidance)
		if !errors.Is(result.err, task.ErrLockBusy) || !errors.Is(result.err, context.DeadlineExceeded) || !workspacescore.PendingError(result.err) {
			t.Fatalf("pending observation command error = %v; want the whole pending deadline", result.err)
		}
	})
}

// An explicit task ID leaves the record unknown, so the same pending
// observation must not claim not-created recovery.
func TestManagedDispatchObservationDeadlineWithExplicitIDStaysUnknown(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	synctest.Test(t, func(t *testing.T) {
		observed := dispatchHeldObservation(t, fixture, explicitDispatchTaskID)
		if observed.TaskID != explicitDispatchTaskID || observed.RecordPresence() != tasklifecycle.RecordUnknown {
			t.Fatalf("explicit held observation = task %q, record %v; want the unknown explicit record", observed.TaskID, observed.Record)
		}
		result := requireDispatchGuidance(t, "explicit observer deadline", observed, unknownRecordGuidance)
		if !errors.Is(result.err, task.ErrLockBusy) || !errors.Is(result.err, context.DeadlineExceeded) || !workspacescore.PendingError(result.err) {
			t.Fatalf("explicit observation command error = %v; want the whole pending deadline", result.err)
		}
	})
}

// observationOwnerDeadlineFault parks the admission observer's first
// lifetime-owner acquisition of the helper on a bubble timer past the
// observer's budget, then fails that owner's unlock once. Acquisition then
// detects the observer's deadline and rolls back by closing the owner, whose
// unlock fails: no lease is returned and no intent is read, so the deadline
// is joined with that concrete owner-unlock fault.
type observationOwnerDeadlineFault struct {
	taskdir.BaseFaultInjector
	load          observationLoad
	parked, fired bool
}

func (f *observationOwnerDeadlineFault) OnLockParentDirBarrier(string) error { return nil }
func (f *observationOwnerDeadlineFault) OnLockFileBarrier(path string) error {
	f.load.opened(path)
	return nil
}

func (f *observationOwnerDeadlineFault) OnFlock(path string, operation int) error {
	if f.load.opens != 2 || scheduledHelperJournal(path, ".lifetime-owner.lock") != f.load.helperID {
		return nil
	}
	if operation&unix.LOCK_EX != 0 && !f.parked {
		f.parked = true
		time.Sleep(workspace.DefaultBudget + observerExpiryMargin)
		return nil
	}
	if operation == unix.LOCK_UN && f.parked && !f.fired {
		f.fired = true
		return errOperationOwnerUnlock
	}
	return nil
}

// The failed owner release keeps its lock until the child process exits; the
// parent then proves every recorded operation owner free.
func TestManagedDispatchObservationDeadlineKeepsJoinedFault(t *testing.T) {
	fixture, child := ownerReleaseRuntimeFixture(t)
	if !child {
		return
	}
	synctest.Test(t, func(t *testing.T) {
		fault := &observationOwnerDeadlineFault{load: observationLoad{t: t, adds: fixture.adds}}
		observed := provisionDispatch{runtime: fixture.runtime, fault: fault, budget: observedHelperBudget}.run(fixture)
		fault.load.requireObserved(t, observed)
		if !fault.parked || !fault.fired || observed.RecordPresence() != tasklifecycle.RecordAbsent {
			t.Fatalf("owner fault parked %t, fired %t, record %v; want the observer's release fault before any record", fault.parked, fault.fired, observed.Record)
		}
		result := requireDispatchGuidance(t, "joined observer fault", observed, preparationFaultGuidance)
		for _, err := range []error{observed.Failure, result.err} {
			if !errors.Is(err, errOperationOwnerUnlock) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, workspace.ErrPending) || workspacescore.PendingError(err) {
				t.Fatalf("joined observer fault = %v; want the deadline joined with the release fault, not pending", err)
			}
		}
	})
}

func TestManagedDispatchAbortKeepsOwnerFaultJoinedWithPending(t *testing.T) {
	fixture, child := ownerReleaseRuntimeFixture(t)
	if !child {
		return
	}
	fault := &armedOwnerUnlockFault{}
	observed := dispatchRefusedProvision(t, fixture, fault, true)
	abortErr := observed.Workspace.AbortError
	if !fault.fired || !errors.Is(abortErr, workspace.ErrPending) || !errors.Is(abortErr, errOperationOwnerUnlock) || workspacescore.PendingError(abortErr) {
		t.Fatalf("abort recovery = %v (fired %t); want pending joined with the release fault", abortErr, fault.fired)
	}
	result := dispatchCommandResult(observed)
	if !errors.Is(observed.Err(), errOperationOwnerUnlock) || !errors.Is(result.err, errOperationOwnerUnlock) || !errors.Is(result.err, errFixtureSubmissionRefused) {
		t.Fatalf("dispatch %v, command %+v; want the release fault beside the original failure", observed.Err(), result)
	}
}

func TestManagedDispatchAbortPendingOnlyStaysSuppressed(t *testing.T) {
	observed := dispatchRefusedProvision(t, newCheckpointRuntimeFixture(t), &armedOwnerUnlockFault{}, false)
	if abortErr := observed.Workspace.AbortError; abortErr != nil {
		t.Fatalf("pending abort recovery = %v; want it suppressed", abortErr)
	}
	if result := dispatchCommandResult(observed); !errors.Is(result.err, errFixtureSubmissionRefused) {
		t.Fatalf("command %+v; want the original failure", result)
	}
}
