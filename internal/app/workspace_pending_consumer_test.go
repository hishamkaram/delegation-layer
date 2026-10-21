package app

import (
	"context"
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

// watchStatus runs the public status action and its projection over the
// fixture's production store, as workspaceCommand does under its use owner.
func watchStatus(t *testing.T, fixture *workspacefixture.RecoveryFixture, operationID string) commandResult {
	t.Helper()
	a := Arguments{WorkspaceAction: "status", Watch: watchDuration, WorkspaceOperationID: operationID}
	ctx, cancel := context.WithTimeout(t.Context(), a.Watch)
	defer cancel()
	ack, err := runWorkspaceCommandAction(ctx, a, fixture.Store, fixture.Resource, Dependencies{}, time.Minute)
	return workspaceCommandActionResponse(ctx, fixture.Store, fixture.Resource, a.WorkspaceAction, false, ack, newResponse("workspace"), err)
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
			result := watchStatus(t, fixture, selector)
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
	synctest.Test(t, func(t *testing.T) {
		for _, selector := range watchSelectors {
			fixture := newPendingAdmissionUse(t)
			started := time.Now()
			result := watchStatus(t, fixture, selector)
			if result.code != 3 || result.response.Workspace == nil || result.response.Workspace.NextAction != "watch_workspace" || result.response.Workspace.OperationID != selector {
				t.Fatalf("pending watch with selector %q = %+v; want exit 3", selector, result)
			}
			if elapsed := time.Since(started); elapsed != watchDuration {
				t.Fatalf("pending watch with selector %q returned after %v, want exactly its %v watch", selector, elapsed, watchDuration)
			}
		}
	})
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
