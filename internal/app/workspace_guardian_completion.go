package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func reconcileWorkspaceAfterGuardian(store *taskdir.Store, parsed runnerArguments, deps Dependencies) (resultErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	var workspaceID string
	var err error
	if !parsed.WorkspaceOperation {
		workspaceID, err = workspaceForGuardian(ctx, store, parsed)
		if err != nil || workspaceID == "" {
			return err
		}
	}
	options := workspaceRuntimeOptions(deps)
	resources := &workspaceCommandResources{store: store}
	defer func() { resultErr = errors.Join(resultErr, resources.closeAfter(resultErr)) }()
	if parsed.WorkspaceOperation {
		workspaceID, err = workspaceForGuardian(ctx, store, parsed)
		if err != nil || workspaceID == "" {
			return err
		}
	}
	resource, err := workspace.LoadResource(ctx, store, workspaceID)
	resources.resource = resource
	if err != nil {
		return err
	}
	// The observer currently awaiting this helper can hold the use lease.
	// Durable obligations let that observer or a later wake finish recovery;
	// the completed guardian must not wait on its own caller's lease.
	owner, err := resource.TryAcquireUseOwner(ctx)
	resources.useOwner = owner
	if owner != nil && err != nil {
		return err
	}
	if errors.Is(err, task.ErrLockBusy) && workspaceViewRetryable(err) {
		return workspace.ErrPending
	}
	if err != nil {
		return err
	}
	return workspacescore.ReconcileUse(ctx, store, resource, options)
}

func workspaceForGuardian(ctx context.Context, store *taskdir.Store, parsed runnerArguments) (id string, resultErr error) {
	if parsed.WorkspaceOperation {
		operation, err := workspace.LoadOperation(ctx, store, parsed.TaskID)
		if err != nil {
			return "", err
		}
		defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
		// Ordinary checkpoints and removals own no workspace use. Their
		// coordinator finalizer only publishes the qualified receipt.
		if operation.Request().WorkspaceID == "" {
			return "", workspacescore.FinalizeCheckpoint(ctx, store, operation)
		}
		return operation.Request().WorkspaceID, nil
	}
	td, err := store.OpenTask(parsed.TaskID)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	request, _, err := td.PreparedRecords()
	if err != nil {
		return "", err
	}
	if request.Workspace == nil {
		return "", nil
	}
	return request.Workspace.ID, nil
}
