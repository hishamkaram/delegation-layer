package app

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func workspaceCommand(a Arguments, deps Dependencies) (result commandResult) {
	return workspaceCommandContext(context.Background(), a, deps)
}

func workspaceCommandContext(parent context.Context, a Arguments, deps Dependencies) (result commandResult) {
	deps = deps.withCapturedEnvironmentContext(parent)
	response := newResponse("workspace")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 2)
	}
	store, err := openStoreContext(parent, root, deps.storeDependencies(), false)
	if err != nil {
		return failed(response, err, 1)
	}
	resources := &workspaceCommandResources{store: store, ownsStore: true}
	defer func() { mergeCommandClose(&result, func() error { return resources.closeAfter(result.err) }) }()
	response.RootID = store.RootID
	if a.WorkspaceAction == "list" {
		return listManagedWorkspacesContext(parent, a, store, response)
	}
	budget, err := workspacescore.ParseBudget(a.WorkspaceBudget)
	if err != nil {
		return failed(response, err, 2)
	}
	observationBudget := workspaceObservationBudget(a.WorkspaceAction, a.Watch, budget)
	ctx, cancel := context.WithTimeout(parent, observationBudget)
	defer cancel()
	options := workspaceRuntimeOptions(deps)
	resource, err := workspace.LoadResource(ctx, store, a.WorkspaceID)
	resources.resource = resource
	if err != nil {
		return failed(response, err, 1)
	}
	if a.WorkspaceBudget == "" {
		budget = time.Duration(resource.Request().BudgetNanos)
	}
	if a.WorkspaceAction != "status" {
		if err = options.ValidateGitEnvironment(); err != nil {
			return failed(response, err, 2)
		}
	}
	owner, err := resource.TryAcquireUseOwner(ctx)
	resources.useOwner = owner
	if owner != nil && err != nil {
		return failed(response, err, 1)
	}
	if errors.Is(err, task.ErrLockBusy) && workspaceViewRetryable(err) {
		return workspaceCommandActionResponse(ctx, store, resource, a.WorkspaceAction, a.Discard, workspacescore.RequestedAction(a.WorkspaceOperationID), response, workspace.ErrBusy)
	}
	if err != nil {
		return failed(response, err, 1)
	}
	ack, actionErr := runWorkspaceCommandAction(ctx, a, store, resource, options, budget)
	return workspaceCommandActionResponse(ctx, store, resource, a.WorkspaceAction, a.Discard, ack, response, actionErr)
}

func workspaceObservationBudget(action string, watch, operationBudget time.Duration) time.Duration {
	if action != "status" {
		return operationBudget
	}
	if watch == 0 {
		return time.Second
	}
	return watch
}

func runWorkspaceCommandAction(ctx context.Context, a Arguments, store *taskdir.Store, resource *workspace.Resource, options workspacescore.OperationOptions, budget time.Duration) (workspacescore.ActionAcknowledgement, error) {
	switch a.WorkspaceAction {
	case "status":
		return workspacescore.RequestedAction(a.WorkspaceOperationID), watchManagedWorkspace(ctx, store, resource, options, a)
	case "exec":
		return beginWorkspaceVerification(ctx, store, resource, options, a.ExecArguments, budget)
	case "release":
		return beginWorkspaceRelease(ctx, store, resource, options, a.Discard, budget)
	case "forget":
		return beginWorkspaceForget(ctx, store, resource, options, budget)
	default:
		return workspacescore.ActionAcknowledgement{}, task.ErrInvalidPermit
	}
}

func workspaceCommandResponse(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, operationID string, response Response, err error) commandResult {
	responseContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	description, descriptionErr := workspacescore.Describe(responseContext, resource)
	if operationID != "" {
		description.OperationID = operationID
		descriptionErr = errors.Join(descriptionErr, workspacescore.DescribeOperation(responseContext, store, resource, operationID, &description))
	}
	response.Workspace = &description
	return finishWorkspaceCommandResponse(responseContext, response, err, descriptionErr)
}

func finishWorkspaceCommandResponse(ctx context.Context, response Response, actionErr, descriptionErr error) commandResult {
	if actionErr != nil && !workspacescore.PendingError(actionErr) {
		return failed(response, errors.Join(actionErr, descriptionErr), 1)
	}
	if descriptionErr != nil {
		if expiredWorkspaceView(ctx, descriptionErr) {
			response.Workspace = &WorkspaceResponse{ID: response.Workspace.ID, Isolation: "git-worktree", State: "undetermined", EvidenceState: "unknown"}
			return pendingWorkspaceCommandResponse(response)
		}
		return failed(response, errors.Join(actionErr, descriptionErr), 1)
	}
	if actionErr != nil {
		return pendingWorkspaceCommandResponse(response)
	}
	return commandResult{response: response}
}

func pendingWorkspaceCommandResponse(response Response) commandResult {
	response.Workspace.NextAction = "watch_workspace"
	return commandResult{response: response, code: 3}
}

// expiredWorkspaceView reports whether err is only the observer's own expired
// view: ctx reached its deadline and every cause is retryable. A live or
// cancelled context, or a concrete fault joined with a deadline, is not.
func expiredWorkspaceView(ctx context.Context, err error) bool {
	return errors.Is(ctx.Err(), context.DeadlineExceeded) && workspaceViewRetryable(err)
}

// Only this observer's expired view is retryable. An identity or storage fault
// joined with a deadline remains a failure rather than being hidden as pending.
func workspaceViewRetryable(err error) bool {
	var lifetimeCleanup *taskdir.LifetimeOwnerCleanupError
	var admissionCleanup *workspacescore.AdmissionCleanupError
	var commandCleanup *workspaceCommandCleanupError
	if errors.As(err, &lifetimeCleanup) || errors.As(err, &admissionCleanup) || errors.As(err, &commandCleanup) {
		return false
	}
	return task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, task.ErrLockBusy)
	})
}

func listManagedWorkspacesContext(parent context.Context, a Arguments, store *taskdir.Store, response Response) (result commandResult) {
	if a.WorkspaceLimit == 0 {
		a.WorkspaceLimit = defaultWorkspacePageSize
	}
	if a.WorkspaceLimit < 1 || a.WorkspaceLimit > defaultWorkspacePageSize {
		return failed(response, task.ErrInvalidPermit, 2)
	}
	ctx, cancel := context.WithTimeout(parent, workspace.DefaultBudget)
	defer cancel()
	ids, err := workspace.ResourceIDs(ctx, store)
	if err != nil {
		return failed(response, err, 1)
	}
	for _, id := range ids {
		if id <= a.WorkspaceAfter {
			continue
		}
		if len(response.Workspaces) == a.WorkspaceLimit {
			response.WorkspaceCursor = response.Workspaces[len(response.Workspaces)-1].ID
			break
		}
		resource, loadErr := workspace.LoadResource(ctx, store, id)
		if loadErr != nil {
			return failed(response, loadErr, 1)
		}
		description, describeErr := workspacescore.Describe(ctx, resource)
		closeErr := resource.Close()
		if err = errors.Join(describeErr, closeErr); err != nil {
			return failed(response, err, 1)
		}
		response.Workspaces = append(response.Workspaces, workspaceListSummary(description))
	}
	return commandResult{response: response}
}

func watchManagedWorkspace(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, options workspacescore.OperationOptions, a Arguments) (resultErr error) {
	defer func() { resultErr = workspacescore.PendingAtDeadline(ctx, resultErr) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := observeManagedWorkspace(ctx, store, resource, options, a.WorkspaceOperationID)
		if err == nil || !errors.Is(err, workspace.ErrPending) || !workspacescore.PendingError(err) || a.Watch == 0 {
			return err
		}
		if waitErr := workspacescore.WaitObservation(ctx); waitErr != nil {
			return errors.Join(workspace.ErrPending, waitErr)
		}
	}
}

func observeManagedWorkspace(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, options workspacescore.OperationOptions, operationID string) (resultErr error) {
	useErr := workspacescore.ReconcileUse(ctx, store, resource, options)
	if useErr != nil && (!errors.Is(useErr, workspace.ErrPending) || !workspacescore.PendingError(useErr)) {
		return useErr
	}
	if operationID == "" {
		return useErr
	}
	operation, err := workspace.LoadOperation(ctx, store, operationID)
	if err != nil {
		return errors.Join(useErr, err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if operation.Request().WorkspaceID != resource.Request().WorkspaceID {
		return task.ErrIdentityMismatch
	}
	return errors.Join(useErr, workspacescore.OperationOutcome(ctx, store, operation))
}

func beginWorkspaceVerification(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, options workspacescore.OperationOptions, arguments []string, budget time.Duration) (ack workspacescore.ActionAcknowledgement, resultErr error) {
	return workspacescore.BeginVerification(ctx, store, resource, options, arguments, budget)
}
