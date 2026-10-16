package app

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func workspaceCommand(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("workspace")
	root, err := resolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 2)
	}
	store, err := openStore(root, deps.storeDependencies(), false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	response.RootID = store.RootID
	if a.WorkspaceAction == "list" {
		return listManagedWorkspaces(a, store, response)
	}
	budget, err := parseWorkspaceBudget(a.WorkspaceBudget)
	if err != nil {
		return failed(response, err, 2)
	}
	observationBudget := budget
	if a.WorkspaceAction == "status" {
		observationBudget = a.Watch
		if observationBudget == 0 {
			observationBudget = time.Second
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), observationBudget)
	defer cancel()
	resource, err := workspace.LoadResource(ctx, store, a.WorkspaceID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, resource.Close) }()
	if a.WorkspaceBudget == "" {
		budget = time.Duration(resource.Request().BudgetNanos)
	}
	if a.WorkspaceAction != "status" {
		if err = workspace.ValidateGitEnvironment(); err != nil {
			return failed(response, err, 2)
		}
	}
	owner, err := resource.TryAcquireUseOwner(ctx)
	if errors.Is(err, task.ErrLockBusy) {
		return workspaceCommandActionResponse(ctx, store, resource, a.WorkspaceAction, a.Discard, workspaceActionAcknowledgement{operationID: a.WorkspaceOperationID}, response, workspace.ErrBusy)
	}
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, owner.Release) }()
	ack, actionErr := runWorkspaceCommandAction(ctx, a, store, resource, deps, budget)
	return workspaceCommandActionResponse(ctx, store, resource, a.WorkspaceAction, a.Discard, ack, response, actionErr)
}

func runWorkspaceCommandAction(ctx context.Context, a Arguments, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, budget time.Duration) (workspaceActionAcknowledgement, error) {
	switch a.WorkspaceAction {
	case "status":
		return workspaceActionAcknowledgement{operationID: a.WorkspaceOperationID}, watchManagedWorkspace(ctx, store, resource, deps, a)
	case "exec":
		return beginWorkspaceVerification(ctx, store, resource, deps, a.ExecArguments, budget)
	case "release":
		return beginWorkspaceRelease(ctx, store, resource, deps, a.Discard, budget)
	case "forget":
		return beginWorkspaceForget(ctx, store, resource, deps, budget)
	default:
		return workspaceActionAcknowledgement{}, task.ErrInvalidPermit
	}
}

func workspaceCommandResponse(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, operationID string, response Response, err error) commandResult {
	responseContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	description, descriptionErr := describeWorkspace(responseContext, resource)
	if operationID != "" {
		description.OperationID = operationID
		descriptionErr = errors.Join(descriptionErr, describeWorkspaceOperation(responseContext, store, resource, operationID, &description))
	}
	response.Workspace = &description
	return finishWorkspaceCommandResponse(responseContext, response, err, descriptionErr)
}

func finishWorkspaceCommandResponse(ctx context.Context, response Response, actionErr, descriptionErr error) commandResult {
	if actionErr != nil && !workspacePendingError(actionErr) {
		return failed(response, errors.Join(actionErr, descriptionErr), 1)
	}
	if descriptionErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && workspaceViewRetryable(descriptionErr) {
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

// Only this observer's expired view is retryable. An identity or storage fault
// joined with a deadline remains a failure rather than being hidden as pending.
func workspaceViewRetryable(err error) bool {
	return isPureErrorSet(err, func(cause error) bool {
		return errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, task.ErrLockBusy)
	})
}

func listManagedWorkspaces(a Arguments, store *taskdir.Store, response Response) (result commandResult) {
	if a.WorkspaceLimit == 0 {
		a.WorkspaceLimit = defaultWorkspacePageSize
	}
	if a.WorkspaceLimit < 1 || a.WorkspaceLimit > defaultWorkspacePageSize {
		return failed(response, task.ErrInvalidPermit, 2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
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
		description, describeErr := describeWorkspace(ctx, resource)
		closeErr := resource.Close()
		if err = errors.Join(describeErr, closeErr); err != nil {
			return failed(response, err, 1)
		}
		response.Workspaces = append(response.Workspaces, workspaceListSummary(description))
	}
	return commandResult{response: response}
}

func watchManagedWorkspace(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, a Arguments) (resultErr error) {
	defer func() {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && errors.Is(resultErr, context.DeadlineExceeded) {
			resultErr = errors.Join(workspace.ErrPending, resultErr)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := observeManagedWorkspace(ctx, store, resource, deps, a.WorkspaceOperationID)
		if err == nil || !errors.Is(err, workspace.ErrPending) || a.Watch == 0 {
			return err
		}
		if waitErr := waitWorkspaceObservation(ctx); waitErr != nil {
			return workspace.ErrPending
		}
	}
}

func observeManagedWorkspace(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, operationID string) (resultErr error) {
	useErr := reconcileWorkspaceUse(ctx, store, resource, deps)
	if useErr != nil && !errors.Is(useErr, workspace.ErrPending) {
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
	_, _, err = acceptedWorkspaceOperation(ctx, store, operation)
	return errors.Join(useErr, err)
}

func beginWorkspaceVerification(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, arguments []string, budget time.Duration) (ack workspaceActionAcknowledgement, resultErr error) {
	if err := reconcileWorkspaceUse(ctx, store, resource, deps); err != nil {
		return ack, err
	}
	id, err := task.NewTaskID()
	if err != nil {
		return ack, err
	}
	data, err := task.MarshalCanonical(struct {
		WorkspaceID string   `json:"workspace_id"`
		ID          string   `json:"id"`
		Arguments   []string `json:"arguments"`
	}{resource.Request().WorkspaceID, id, arguments})
	if err != nil {
		return ack, err
	}
	authorize := func(scheduling context.Context, operation *workspace.Operation) error {
		ack.operationID = operation.Request().OperationID
		subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: ack.operationID, SHA256: operation.Digest()}
		reservation, reserveErr := resource.Reserve(scheduling, subject)
		if reserveErr != nil {
			return reserveErr
		}
		ack.authorized = true
		return recordWorkspaceVerificationUse(scheduling, resource, reservation, operation)
	}
	operation, err := scheduleWorkspaceOperation(ctx, store, resource, deps, workspaceOperationSpec{purpose: "verify-" + id + ".json", kind: workspace.OperationVerify, subject: workspace.Subject{Kind: workspace.SubjectAdmission, ID: id, SHA256: task.ComputeSHA256(data)}, budget: budget, arguments: arguments}, authorize)
	defer func() { resultErr = errors.Join(resultErr, closeWorkspaceOperation(operation)) }()
	if err != nil {
		return ack, err
	}
	return ack, workspace.ErrPending
}

func describeWorkspaceOperation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, operationID string, response *WorkspaceResponse) (resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, operationID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if operation.Request().WorkspaceID != resource.Request().WorkspaceID {
		return task.ErrIdentityMismatch
	}
	result, _, err := acceptedWorkspaceOperation(ctx, store, operation)
	if errors.Is(err, errWorkspaceOperationResultUnavailable) {
		response.ReasonCode = "operation_result_unavailable"
	}
	if result.OperationID != "" {
		response.Operation = &result
		response.ReasonCode = result.ReasonCode
	}
	if errors.Is(err, workspace.ErrPending) {
		response.NextAction = "watch_workspace"
		return nil
	}
	if err != nil {
		return err
	}
	appendWorkspaceArtifactResponses(store.Root, operationID, result.Artifacts, response)
	return nil
}
