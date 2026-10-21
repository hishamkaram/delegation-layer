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

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

// These cases compose the checkpoint coordinator, the production operation
// journals, the fake Pueue supervisor's receipt-before-enqueue submission,
// the in-process checkpoint action over native Git inside a production
// guardian record lifetime, and guardian completion. They do not execute the
// Pueue-launched worker child or a provider.

type checkpointRuntimeHooks struct{ prepare func() }

// checkpointTestRuntime resolves the test binary as the managed runner and
// reopens the fixture supervisor binding for every scheduling transaction.
type checkpointTestRuntime struct {
	saved   workspaces.OperationRuntime
	runner  string
	binding task.SupervisorRef
	hooks   *checkpointRuntimeHooks
}

func (r checkpointTestRuntime) ManagedRunner() (string, error) { return r.runner, nil }

func (r checkpointTestRuntime) PrepareManaged(ctx context.Context, store *taskdir.Store, runner string) (string, *pueue.Client, error) {
	if hook := r.hooks.prepare; hook != nil {
		hook()
	}
	client, err := r.saved.OpenSaved(ctx, store.Root, r.binding)
	return runner, client, err
}

func (r checkpointTestRuntime) OpenSaved(ctx context.Context, root string, saved task.SupervisorRef) (*pueue.Client, error) {
	return r.saved.OpenSaved(ctx, root, saved)
}

type checkpointRuntimeFixture struct {
	lineage nativeLineage
	deps    Dependencies
	options workspaces.OperationOptions
	runtime checkpointTestRuntime
	hooks   *checkpointRuntimeHooks
	adds    *atomic.Int32
}

func newCheckpointRuntimeFixture(t *testing.T) checkpointRuntimeFixture {
	t.Helper()
	return newCheckpointRuntimeFixtureOf(t, newNativeLineage(t))
}

// ownerReleaseRuntimeFixture runs the calling test's fault half in a child
// process, because a failed owner release keeps its locked descriptor until
// exit. Supervisor and runtime handles cannot cross processes, so the child
// builds the fixture inside the parent-owned base. The parent returns false
// after proving every recorded operation owner free.
func ownerReleaseRuntimeFixture(t *testing.T) (checkpointRuntimeFixture, bool) {
	t.Helper()
	base := workspacefixture.NewOwnerReleaseBase(t)
	if !base.Child() {
		base.RunChild(t, nativeLineageStateRoot(base.Dir))
		return checkpointRuntimeFixture{}, false
	}
	return newCheckpointRuntimeFixtureOf(t, newNativeLineageIn(t, base.Dir, "sha1")), true
}

func newCheckpointRuntimeFixtureOf(t *testing.T, lineage nativeLineage) checkpointRuntimeFixture {
	t.Helper()
	supervisor, _ := newTerminalEventInspectionSupervisor(t, lineage.store)
	adds := &atomic.Int32{}
	observer := func(event pueue.CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "add") && !slices.Contains(event.Argv, "--help") {
			adds.Add(1)
		}
	}
	deps := Dependencies{InitialSupervisorExecutable: supervisor.Binding().ClientExecutable, SupervisorOptions: pueue.Options{Environment: os.Environ(), Observer: observer}}
	options := workspaceRuntimeOptions(deps)
	hooks := &checkpointRuntimeHooks{}
	runtime := checkpointTestRuntime{saved: options.Runtime, runner: executablePath(t), binding: supervisor.Binding(), hooks: hooks}
	options.Runtime = runtime
	return checkpointRuntimeFixture{lineage: lineage, deps: deps, options: options, runtime: runtime, hooks: hooks, adds: adds}
}

func (f checkpointRuntimeFixture) ctx() context.Context { return f.lineage.scope.ctx }

func (f checkpointRuntimeFixture) requireAdds(t *testing.T, want int32) {
	t.Helper()
	if got := f.adds.Load(); got != want {
		t.Fatalf("supervisor adds = %d, want %d", got, want)
	}
}

// beginOrdinary canonicalizes cwd through the existing configuration owner
// and schedules one checkpoint of it.
func (f checkpointRuntimeFixture) beginOrdinary(t *testing.T, cwd string) string {
	t.Helper()
	_, canonical, err := config.ValidateDirectories(f.lineage.store.Root, cwd)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := workspaces.OrdinaryCheckpointSelection(canonical)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := workspaces.BeginOrdinaryCheckpoint(f.ctx(), f.lineage.store, f.options, selection, time.Minute)
	return requireAcceptedPending(t, "ordinary checkpoint", ack, err)
}

// runHelper performs operationID's checkpoint action exactly as the worker's
// supervised scope does, inside a production guardian record lifetime, then
// runs guardian completion. act=false closes the lifetime with no result.
func (f checkpointRuntimeFixture) runHelper(t *testing.T, operationID string, act bool) {
	t.Helper()
	store := f.lineage.store
	operation, err := workspace.LoadOperation(f.ctx(), store, operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	workspacefixture.RunRealGuardianLifetime(t, store, operation, func() {
		if act {
			f.performAction(t, operation)
		}
	})
	if err = reconcileWorkspaceAfterGuardian(store, runnerArguments{WorkspaceOperation: true, TaskID: operationID}, f.deps); err != nil {
		t.Fatalf("guardian completion: %v", err)
	}
}

func (f checkpointRuntimeFixture) performAction(t *testing.T, operation *workspace.Operation) {
	t.Helper()
	store := f.lineage.store
	var candidate workspace.OperationResult
	workErr, stopErr := execution.RunSupervised(operation.Deadline(), execution.SupervisedOptions{Stopper: workspaceSignalTestStopper{}}, func(scope execution.PreflightScope) error {
		var actionErr error
		candidate, actionErr = runWorkspaceAction(scope, store, operation)
		return actionErr
	})
	if stopErr != nil {
		t.Fatal(stopErr)
	}
	if workErr != nil {
		// The worker's failure path keeps the known cause in its error.
		if err := recordWorkspaceWorkerFailure(f.ctx(), store, operation, workErr); !errors.Is(err, workErr) {
			t.Fatalf("worker failure was not retained: %v", err)
		}
		return
	}
	if err := recordWorkspaceWorkerResult(f.ctx(), store, operation, candidate); err != nil {
		t.Fatal(err)
	}
}

func (f checkpointRuntimeFixture) observe(t *testing.T, checkpointID string) workspaces.CheckpointDescription {
	t.Helper()
	description, err := workspaces.ObserveCheckpoint(f.ctx(), f.lineage.store, f.options, checkpointID)
	if err != nil {
		t.Fatalf("observe checkpoint: %v", err)
	}
	return description
}

func (f checkpointRuntimeFixture) attempts(t *testing.T, checkpointID string) []workspace.CheckpointRemovalReference {
	t.Helper()
	original, err := workspace.LoadOperation(f.ctx(), f.lineage.store, checkpointID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, attemptsErr := original.CheckpointRemovalAttempts(f.ctx())
	if err = errors.Join(attemptsErr, original.Close()); err != nil {
		t.Fatal(err)
	}
	return attempts
}

func (f checkpointRuntimeFixture) beginRemoval(t *testing.T, checkpointID string) (workspaces.ActionAcknowledgement, error) {
	t.Helper()
	return workspaces.BeginCheckpointRemoval(f.ctx(), f.lineage.store, f.options, checkpointID, time.Minute)
}

// requireAcceptedPending asserts an authorized acknowledgement whose helper
// is still pending, and returns its operation ID.
func requireAcceptedPending(t *testing.T, action string, ack workspaces.ActionAcknowledgement, err error) string {
	t.Helper()
	if !ack.Authorized() || ack.OperationID() == "" || !errors.Is(err, workspace.ErrPending) {
		t.Fatalf("%s acknowledgement = %+v, %v; want accepted pending", action, ack, err)
	}
	return ack.OperationID()
}

// requireCheckpointRef asserts the described checkpoint's ref names its commit
// in the shared repository.
func (f checkpointRuntimeFixture) requireCheckpointRef(t *testing.T, described workspaces.CheckpointDescription) {
	t.Helper()
	if got := f.lineage.gitIn(t, f.lineage.main, "rev-parse", "--verify", described.SnapshotRef); got != described.CommitOID {
		t.Fatalf("checkpoint ref %s names %s, want %s", described.SnapshotRef, got, described.CommitOID)
	}
}

// requireRemoved asserts removalID, the attempts-th attempt, removed the
// checkpoint's owned ref.
func (f checkpointRuntimeFixture) requireRemoved(t *testing.T, checkpointID, removalID string, attempts uint64) {
	t.Helper()
	removed := f.observe(t, checkpointID)
	if removed.Usable || removed.Removal == nil || removed.Removal.State != workspace.CheckpointRemoved ||
		removed.Removal.OperationID != removalID || removed.Removal.Attempts != attempts {
		t.Fatalf("removed checkpoint = %+v; want removal %s after %d attempts", removed, removalID, attempts)
	}
	if f.lineage.gitSucceeds(t, f.lineage.main, "rev-parse", "--verify", "--quiet", removed.SnapshotRef) {
		t.Fatal("removal left the owned checkpoint ref")
	}
}

func TestOrdinaryCheckpointCreatesAndRemovesThroughCoordinatorWorkerAndGuardianCompletion(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	main := fixture.lineage.main
	writeLineageFile(t, filepath.Join(main, "tracked.txt"), "checkpoint edit\n")
	head := fixture.lineage.gitIn(t, main, "rev-parse", "HEAD")
	checkpointID := fixture.beginOrdinary(t, main)
	fixture.requireAdds(t, 1)
	fixture.runHelper(t, checkpointID, true)
	created := fixture.observe(t, checkpointID)
	if created.State != workspace.CheckpointCheckpointed || !created.Usable || created.Selection != workspace.CheckpointOrdinary || created.Cwd != main {
		t.Fatalf("created checkpoint = %+v", created)
	}
	fixture.requireCheckpointRef(t, created)
	if got := fixture.lineage.gitIn(t, main, "show", created.CommitOID+":tracked.txt"); got != "checkpoint edit" {
		t.Fatalf("checkpoint content = %q", got)
	}
	if got := fixture.lineage.gitIn(t, main, "rev-parse", "HEAD"); got != head {
		t.Fatalf("checkpoint moved HEAD to %s", got)
	}
	page, cursor, err := workspaces.ListCheckpoints(fixture.ctx(), fixture.lineage.store, "", 10)
	if err != nil || cursor != "" || len(page) != 1 || page[0].ID != checkpointID || !page[0].Usable {
		t.Fatalf("listed checkpoints = %+v, %q, %v", page, cursor, err)
	}
	removal, err := fixture.beginRemoval(t, checkpointID)
	removalID := requireAcceptedPending(t, "removal", removal, err)
	fixture.requireAdds(t, 2)
	fixture.runHelper(t, removalID, true)
	fixture.requireRemoved(t, checkpointID, removalID, 1)
	again, err := fixture.beginRemoval(t, checkpointID)
	if err != nil || again.OperationID() != removalID || len(fixture.attempts(t, checkpointID)) != 1 {
		t.Fatalf("repeated removal = %+v, %v", again, err)
	}
	fixture.requireAdds(t, 2)
}

func TestCheckpointRemovalRaceSubmitsOnlyRegisteredWinnerAndRetriesAfterClosedFailure(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	checkpointID := fixture.beginOrdinary(t, fixture.lineage.main)
	fixture.runHelper(t, checkpointID, true)
	var winner workspaces.ActionAcknowledgement
	var winnerErr error
	// The first caller observes head 0, then the second registers and submits
	// its own attempt while the first is still preparing its request.
	fixture.hooks.prepare = func() {
		fixture.hooks.prepare = nil
		winner, winnerErr = fixture.beginRemoval(t, checkpointID)
	}
	loser, err := fixture.beginRemoval(t, checkpointID)
	winnerID := requireAcceptedPending(t, "winning removal", winner, winnerErr)
	// The loser redrives the winner's registered request. Its receipt names
	// task 9, which this fixture's empty supervisor status cannot observe, so
	// the whole error is a concrete observation fault, not accepted-pending.
	if !loser.Authorized() || loser.OperationID() != winnerID || !errors.Is(err, workspace.ErrPending) ||
		!errors.Is(err, pueue.ErrUnknown) || errors.Is(err, workspace.ErrBusy) || workspaces.PendingError(err) {
		t.Fatalf("losing caller = %+v, %v; want the winner's pending attempt %+v", loser, err, winner)
	}
	attempts := fixture.attempts(t, checkpointID)
	if len(attempts) != 1 || attempts[0].OperationID != winnerID {
		t.Fatalf("attempts after race = %+v", attempts)
	}
	fixture.requireAdds(t, 2)
	fixture.requireUnregisteredRemovalNeverSubmitted(t, checkpointID, winnerID)

	fixture.runHelper(t, winnerID, false)
	if failed := fixture.observe(t, checkpointID); failed.Removal == nil || failed.Removal.State != workspace.CheckpointUnavailable ||
		failed.Removal.ReasonCode != workspace.CheckpointRemovalResultUnavailable || failed.Usable {
		t.Fatalf("closed failed attempt = %+v", failed)
	}
	retry, err := fixture.beginRemoval(t, checkpointID)
	retryID := requireAcceptedPending(t, "retry after closed failure", retry, err)
	if retryID == winnerID {
		t.Fatalf("retry reused the closed failed attempt %s", winnerID)
	}
	fixture.requireAdds(t, 3)
	fixture.runHelper(t, retryID, true)
	fixture.requireRemoved(t, checkpointID, retryID, 2)
	fixture.requireExactRemoval(t, checkpointID, workspaces.CheckpointRemovalDescription{OperationID: winnerID, Attempts: 2, State: workspace.CheckpointUnavailable, ReasonCode: workspace.CheckpointRemovalResultUnavailable})
	fixture.requireExactRemoval(t, checkpointID, workspaces.CheckpointRemovalDescription{OperationID: retryID, Attempts: 2, State: workspace.CheckpointRemoved})
}

// requireExactRemoval observes want's own attempt by its acknowledged ID
// beside the latest removal; both count the same registered attempts.
func (f checkpointRuntimeFixture) requireExactRemoval(t *testing.T, checkpointID string, want workspaces.CheckpointRemovalDescription) {
	t.Helper()
	current, selected, err := workspaces.ObserveCheckpointRemoval(f.ctx(), f.lineage.store, f.options, checkpointID, want.OperationID)
	if err != nil || selected == nil || *selected != want || current.Removal == nil || current.Removal.Attempts != want.Attempts || current.Removal.State != workspace.CheckpointRemoved {
		t.Fatalf("exact removal %s = %+v, current %+v, %v; want %+v", want.OperationID, selected, current.Removal, err, want)
	}
}

func (f checkpointRuntimeFixture) requireUnregisteredRemovalNeverSubmitted(t *testing.T, checkpointID, winnerID string) {
	t.Helper()
	ids, err := f.lineage.store.WorkspaceOperationIDs(f.ctx())
	if err != nil {
		t.Fatal(err)
	}
	orphans := 0
	for _, id := range ids {
		if id == checkpointID || id == winnerID {
			continue
		}
		orphans++
		operation, loadErr := workspace.LoadOperation(f.ctx(), f.lineage.store, id)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		_, receiptErr := operation.Receipt(f.ctx())
		actionErr := workspaces.ValidateCheckpointAction(f.ctx(), f.lineage.store, operation)
		if closeErr := operation.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if !workspace.OperationAbsent(receiptErr) || !errors.Is(actionErr, task.ErrIdentityMismatch) {
			t.Fatalf("losing request %s: receipt=%v action=%v", id, receiptErr, actionErr)
		}
	}
	if orphans != 1 {
		t.Fatalf("losing requests = %d, want 1", orphans)
	}
}

func TestCheckpointRemovalUsesOnlyCommonDirectoryAndRefusesItsLoss(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	linked := fixture.lineage.linkedSource(t, filepath.Join(fixture.lineage.root, "linked"))
	transient := fixture.beginOrdinary(t, linked)
	fixture.runHelper(t, transient, true)
	fixture.lineage.gitIn(t, fixture.lineage.main, "worktree", "remove", "--force", linked)
	removal, err := fixture.beginRemoval(t, transient)
	removalID := requireAcceptedPending(t, "removal without its source checkout", removal, err)
	fixture.runHelper(t, removalID, true)
	fixture.requireRemoved(t, transient, removalID, 1)

	retained := fixture.beginOrdinary(t, fixture.lineage.main)
	fixture.runHelper(t, retained, true)
	created := fixture.observe(t, retained)
	commonDir, moved := filepath.Join(fixture.lineage.main, ".git"), filepath.Join(fixture.lineage.root, "moved-git")
	if err = os.Rename(commonDir, moved); err != nil {
		t.Fatal(err)
	}
	refused, err := fixture.beginRemoval(t, retained)
	fixture.runHelper(t, requireAcceptedPending(t, "removal with a missing common directory", refused, err), true)
	if err = os.Rename(moved, commonDir); err != nil {
		t.Fatal(err)
	}
	described := fixture.observe(t, retained)
	if described.Removal == nil || described.Removal.State != workspace.CheckpointUnavailable || described.Removal.ReasonCode != workspace.CheckpointRemovalFailed || described.SnapshotRef == "" {
		t.Fatalf("missing common directory removal = %+v", described)
	}
	fixture.requireCheckpointRef(t, created)
}

func TestManagedCheckpointAcknowledgesOnlyItsReservedRequestAndSettlesOnCompletion(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	store := fixture.lineage.store
	resource, _ := fixture.lineage.provisionedChild(t, fixture.lineage.main)
	foreign := lineageOperationSubject("foreign use")
	fixture.hooks.prepare = func() {
		fixture.hooks.prepare = nil
		if _, err := resource.Reserve(fixture.ctx(), foreign); err != nil {
			t.Error(err)
		}
	}
	refused, err := workspaces.BeginManagedCheckpoint(fixture.ctx(), store, resource, fixture.options, time.Minute)
	if refused.Authorized() || !errors.Is(err, workspace.ErrBusy) {
		t.Fatalf("checkpoint of a reserved workspace = %+v, %v; want unauthorized busy", refused, err)
	}
	if ids, idsErr := resource.OperationIDs(fixture.ctx()); idsErr != nil || slices.Contains(ids, refused.OperationID()) {
		t.Fatalf("failed Reserve left a resource reference: %v, %v", ids, idsErr)
	}
	fixture.requireAdds(t, 0)
	reservation, err := resource.Reservation(fixture.ctx())
	if err != nil || reservation.Subject != foreign {
		t.Fatalf("busy refusal changed the reservation: %+v, %v", reservation, err)
	}
	if err = resource.CloseReservation(fixture.ctx(), reservation, "operation-completed", ""); err != nil {
		t.Fatal(err)
	}

	accepted, err := workspaces.BeginManagedCheckpoint(fixture.ctx(), store, resource, fixture.options, time.Minute)
	checkpointID := requireAcceptedPending(t, "managed checkpoint", accepted, err)
	fixture.requireAdds(t, 1)
	fixture.runHelper(t, checkpointID, true)
	if _, err = resource.Reservation(fixture.ctx()); !workspace.OperationAbsent(err) {
		t.Fatalf("completed checkpoint kept the workspace reserved: %v", err)
	}
	described := fixture.observe(t, checkpointID)
	if described.State != workspace.CheckpointCheckpointed || described.WorkspaceID != resource.Request().WorkspaceID || !described.Usable {
		t.Fatalf("managed checkpoint = %+v", described)
	}
	fixture.requireCheckpointRef(t, described)
}

// acceptManagedCheckpoint accepts a managed checkpoint of a provisioned
// child whose helper has not published lifetime intent.
func acceptManagedCheckpoint(t *testing.T, fixture checkpointRuntimeFixture) (*workspace.Resource, string) {
	t.Helper()
	resource, _ := fixture.lineage.provisionedChild(t, fixture.lineage.main)
	accepted, err := workspaces.BeginManagedCheckpoint(fixture.ctx(), fixture.lineage.store, resource, fixture.options, time.Minute)
	return resource, requireAcceptedPending(t, "managed checkpoint", accepted, err)
}

// TestManagedCheckpointResponseKeepsOwnerFaultJoinedWithPending describes an
// accepted managed checkpoint whose helper has not published lifetime intent.
func TestManagedCheckpointResponseKeepsOwnerFaultJoinedWithPending(t *testing.T) {
	fixture, child := ownerReleaseRuntimeFixture(t)
	if !child {
		return
	}
	store := fixture.lineage.store
	resource, checkpointID := acceptManagedCheckpoint(t, fixture)
	fault := injectOperationOwnerUnlockFault(t, store, checkpointID)
	result := workspaceCommandResponse(fixture.ctx(), store, resource, checkpointID, newResponse("workspace"), workspace.ErrPending)
	requireOperationResponse(t, result, checkpointID, fault)
}

func TestManagedCheckpointResponsePendingOnly(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	resource, checkpointID := acceptManagedCheckpoint(t, fixture)
	result := workspaceCommandResponse(fixture.ctx(), fixture.lineage.store, resource, checkpointID, newResponse("workspace"), workspace.ErrPending)
	requireOperationResponse(t, result, checkpointID, nil)
}
