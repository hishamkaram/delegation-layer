package app

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

// checkpointCommand parses nothing and decides no lifecycle policy: the
// workspaces owner admits, schedules, qualifies, removes and lists, and this
// command projects its acknowledgement and qualified descriptions.
func checkpointCommand(a Arguments, deps Dependencies) commandResult {
	deps = deps.withCapturedEnvironment()
	return runCheckpointCommand(a, deps.storeDependencies(), workspaceRuntimeOptions(deps))
}

// runCheckpointCommand opens the state root. Only ordinary creation may
// initialize it, after its explicit target has been validated.
func runCheckpointCommand(a Arguments, deps storeDependencies, options workspacescore.OperationOptions) (result commandResult) {
	response := newResponse("checkpoint")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return checkpointFault(response, nil, err)
	}
	ordinary := a.CheckpointAction == checkpointCreateAction && a.WorkspaceID == ""
	var selection workspace.CheckpointSelection
	if ordinary {
		if selection, err = ordinaryCheckpointSelection(root, a.Cwd); err != nil {
			return failed(response, err, 2)
		}
	}
	if a.CheckpointAction == checkpointCreateAction || a.CheckpointAction == checkpointRemoveAction {
		if err = options.ValidateGitEnvironment(); err != nil {
			return failed(response, err, 2)
		}
	}
	budget, err := workspacescore.ParseBudget(a.CheckpointBudget)
	if err != nil {
		return failed(response, err, 2)
	}
	store, err := openStore(root, deps, ordinary)
	if err != nil {
		return checkpointFault(response, nil, err)
	}
	defer func() { mergeCheckpointClose(&result, store.Close) }()
	response.RootID = store.RootID
	switch {
	case a.CheckpointAction == "list":
		return listCheckpoints(a, store, response)
	case a.CheckpointAction == "status":
		return watchCheckpoint(a, store, options, response)
	case a.CheckpointAction == checkpointRemoveAction:
		return removeCheckpoint(a.CheckpointID, store, options, budget, response)
	case ordinary:
		return createOrdinaryCheckpoint(selection, store, options, budget, response)
	default:
		return createManagedCheckpoint(a.WorkspaceID, store, options, budget, response)
	}
}

// ordinaryCheckpointSelection canonicalizes cwd through the configuration
// owner, then pins it; a path inside a managed workspace is refused there.
func ordinaryCheckpointSelection(root, cwd string) (workspace.CheckpointSelection, error) {
	_, canonical, err := config.ValidateDirectories(root, cwd)
	if err != nil {
		return workspace.CheckpointSelection{}, err
	}
	return workspacescore.OrdinaryCheckpointSelection(canonical)
}

// mergeCheckpointClose keeps the shared close-fault policy and makes the
// failed response's guidance match its exit.
func mergeCheckpointClose(result *commandResult, closeFn func() error) {
	mergeCommandClose(result, closeFn)
	if result.code != 1 {
		return
	}
	if result.response.Checkpoint == nil {
		result.response.Checkpoint = &CheckpointResponse{}
	}
	result.response.Checkpoint.NextAction = checkpointStopAndReport
}

func createOrdinaryCheckpoint(selection workspace.CheckpointSelection, store *taskdir.Store, options workspacescore.OperationOptions, budget time.Duration, response Response) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	ack, err := workspacescore.BeginOrdinaryCheckpoint(ctx, store, options, selection, budget)
	return observeCheckpointCreation(ctx, store, options, ack, err, response)
}

// createManagedCheckpoint creates under the workspace's use owner. A busy
// owner is an unauthorized refusal: the Lead retries after the current use
// closes; an active parent never waits for itself or bypasses through --cwd.
func createManagedCheckpoint(workspaceID string, store *taskdir.Store, options workspacescore.OperationOptions, budget time.Duration, response Response) (result commandResult) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	resource, err := workspace.LoadResource(ctx, store, workspaceID)
	if err != nil {
		return checkpointFault(response, nil, err)
	}
	defer func() { mergeCheckpointClose(&result, resource.Close) }()
	owner, err := resource.TryAcquireUseOwner(ctx)
	if errors.Is(err, task.ErrLockBusy) && workspaceViewRetryable(err) {
		return observeCheckpointCreation(ctx, store, options, workspacescore.ActionAcknowledgement{}, workspace.ErrBusy, response)
	}
	if err != nil {
		return checkpointFault(response, nil, err)
	}
	defer func() { mergeCheckpointClose(&result, owner.Release) }()
	ack, err := workspacescore.BeginManagedCheckpoint(ctx, store, resource, options, budget)
	return observeCheckpointCreation(ctx, store, options, ack, err, response)
}

// observeCheckpointCreation projects the acknowledgement. Only a creation
// Begin reported complete is observed, so its qualified receipt chooses the
// exit; an accepted pending creation is watched through status.
func observeCheckpointCreation(ctx context.Context, store *taskdir.Store, options workspacescore.OperationOptions, ack workspacescore.ActionAcknowledgement, actionErr error, response Response) commandResult {
	checkpoint, result, done := checkpointAcknowledgement(response, checkpointCreateAction, "", ack, actionErr)
	if done {
		return result
	}
	description, err := workspacescore.ObserveCheckpoint(ctx, store, options, checkpoint.ID)
	return checkpointDescriptionResult(response, checkpoint, description, err, true)
}

// removeCheckpoint begins or reuses the registered removal attempt. When Begin
// reports that attempt complete, exactly that attempt is observed once.
func removeCheckpoint(checkpointID string, store *taskdir.Store, options workspacescore.OperationOptions, budget time.Duration, response Response) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	ack, err := workspacescore.BeginCheckpointRemoval(ctx, store, options, checkpointID, budget)
	checkpoint, result, done := checkpointAcknowledgement(response, checkpointRemoveAction, checkpointID, ack, err)
	if done {
		return result
	}
	current, selected, err := workspacescore.ObserveCheckpointRemoval(ctx, store, options, checkpointID, checkpoint.OperationID)
	return checkpointRemovalResult(response, checkpoint, checkpointID, current, selected, err)
}

// watchCheckpoint repeats the same observer and selector until a settled or
// failed result or the finite watch ends. It never begins an action; each
// observation owns its own control deadline, so watch expiry cannot cancel or
// renew accepted helper work. The latest observation is the result.
func watchCheckpoint(a Arguments, store *taskdir.Store, options workspacescore.OperationOptions, response Response) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), a.Watch)
	defer cancel()
	for {
		result := observeCheckpointOnce(ctx, a, store, options, response)
		if a.Watch == 0 || result.code != 3 {
			return result
		}
		if workspacescore.WaitObservation(ctx) != nil {
			return result
		}
	}
}

func observeCheckpointOnce(ctx context.Context, a Arguments, store *taskdir.Store, options workspacescore.OperationOptions, response Response) commandResult {
	if a.CheckpointOperationID == "" {
		description, err := workspacescore.ObserveCheckpoint(ctx, store, options, a.CheckpointID)
		return checkpointDescriptionResult(response, &CheckpointResponse{}, description, err, false)
	}
	current, selected, err := workspacescore.ObserveCheckpointRemoval(ctx, store, options, a.CheckpointID, a.CheckpointOperationID)
	return checkpointRemovalResult(response, &CheckpointResponse{}, a.CheckpointID, current, selected, err)
}

// listCheckpoints returns one metadata page. It performs no Git, bootstrap,
// submission, intent, fencing or recovery; entries may be pending or
// unavailable without changing the command's success.
func listCheckpoints(a Arguments, store *taskdir.Store, response Response) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	page, cursor, err := workspacescore.ListCheckpoints(ctx, store, a.CheckpointAfter, a.CheckpointLimit)
	if err != nil {
		return checkpointFault(response, nil, err)
	}
	response.Checkpoints, response.CheckpointCursor = &page, cursor
	return commandResult{response: response}
}
