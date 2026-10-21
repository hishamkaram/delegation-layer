package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
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

// dispatchRefusedProvision dispatches an isolated workspace whose provision
// helper is persisted but never submitted, so Finish recovers the admission.
func dispatchRefusedProvision(t *testing.T, fixture checkpointRuntimeFixture, fault *armedOwnerUnlockFault, hooked bool) tasklifecycle.DispatchResult {
	t.Helper()
	options := dispatchOptions(fixture.deps, false)
	options.Workspace = fixture.options
	options.Workspace.Runtime = refusingSubmissionRuntime{checkpointTestRuntime: fixture.runtime, fault: fault}
	options.OpenStore = func(root string, create bool) (*taskdir.Store, error) {
		store, err := openStore(root, fixture.deps.storeDependencies(), create)
		if err == nil && hooked {
			store.SetFaultInjector(fault)
		}
		return store, err
	}
	request := dispatchRequest(Arguments{
		Root: fixture.lineage.store.Root, Cwd: fixture.lineage.main, Provider: "codex:exec", Isolation: "git-worktree", WorkspaceBudget: "1m",
		BriefBytes: []byte("bounded task"), Config: task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m"},
	})
	observed := tasklifecycle.Dispatch(request, options)
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
	options := dispatchOptions(fixture.deps, false)
	options.Workspace = fixture.options
	options.Workspace.Runtime = pendingProvisionRuntime{checkpointTestRuntime: fixture.runtime}
	options.OpenStore = func(root string, create bool) (*taskdir.Store, error) {
		return openStore(root, fixture.deps.storeDependencies(), create)
	}
	request := dispatchRequest(Arguments{
		Root: fixture.lineage.store.Root, Cwd: fixture.lineage.main, Provider: "codex:exec", Isolation: "git-worktree", WorkspaceBudget: "1m",
		BriefBytes: []byte("bounded task"), Config: task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m"},
	})
	observed := tasklifecycle.Dispatch(request, options)
	return observed
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

func assertPendingProvisionWireResponse(t *testing.T, observed tasklifecycle.DispatchResult) {
	t.Helper()
	result := dispatchCommandResult(observed)
	if result.code != 3 || result.response.TaskRecord != "not_created" || result.response.Failure == nil || result.response.Failure.Code != "workspace_provision_pending" || result.response.Failure.NextAction != "watch_workspace" || result.response.Failure.RecoveryCommand != workspaceStatusCommand(observed.Root, observed.Workspace.Description.ID) {
		t.Fatalf("pending provision dispatch projection = %+v", result)
	}
	var output bytes.Buffer
	if err := writeJSONResponse(&output, "dispatch", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TaskRecord != "not_created" || decoded.Failure == nil || decoded.Failure.Code != "workspace_provision_pending" || decoded.Failure.RecoveryCommand != result.response.Failure.RecoveryCommand {
		t.Fatalf("pending provision wire response = %+v", decoded)
	}
}

func assertPendingProvisionProjection(t *testing.T, observed tasklifecycle.DispatchResult) {
	t.Helper()
	assertPendingProvisionState(t, observed)
	assertPendingProvisionWireResponse(t, observed)
}

func assertPendingProvisionProjectionControls(t *testing.T, observed tasklifecycle.DispatchResult) {
	t.Helper()
	for _, tc := range []struct {
		name   string
		mutate func(*tasklifecycle.DispatchResult)
	}{
		{name: "unknown record", mutate: func(candidate *tasklifecycle.DispatchResult) { candidate.Record = tasklifecycle.RecordUnknown }},
		{name: "concrete fault", mutate: func(candidate *tasklifecycle.DispatchResult) {
			candidate.Failure = errors.Join(candidate.Failure, task.ErrEvidenceFault)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := observed
			tc.mutate(&candidate)
			projected := dispatchCommandResult(candidate)
			if tc.name == "unknown record" && projected.response.TaskRecord != "unknown" {
				t.Fatalf("unknown record became %q", projected.response.TaskRecord)
			}
			if projected.code == 3 && projected.response.Failure != nil && projected.response.Failure.Code == "workspace_provision_pending" {
				t.Fatalf("%s received not-created recovery guidance: %+v", tc.name, projected.response)
			}
		})
	}
}

func TestManagedDispatchProvisionPendingSurvivesPublicProjection(t *testing.T) {
	observed := dispatchPendingProvision(t)
	assertPendingProvisionProjection(t, observed)
	assertPendingProvisionProjectionControls(t, observed)
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
