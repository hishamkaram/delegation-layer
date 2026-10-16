package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func reconcileWorkspaceAfterGuardian(store *taskdir.Store, parsed runnerArguments, deps Dependencies) (resultErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	workspaceID, err := workspaceForGuardian(ctx, store, parsed)
	if err != nil || workspaceID == "" {
		return err
	}
	resource, err := workspace.LoadResource(ctx, store, workspaceID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	// The observer currently awaiting this helper can hold the use lease.
	// Durable obligations let that observer or a later wake finish recovery;
	// the completed guardian must not wait on its own caller's lease.
	owner, err := resource.TryAcquireUseOwner(ctx)
	if errors.Is(err, task.ErrLockBusy) {
		return workspace.ErrPending
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Release()) }()
	return reconcileWorkspaceUse(ctx, store, resource, deps)
}

func workspaceForGuardian(ctx context.Context, store *taskdir.Store, parsed runnerArguments) (id string, resultErr error) {
	if parsed.WorkspaceOperation {
		operation, err := workspace.LoadOperation(ctx, store, parsed.TaskID)
		if err != nil {
			return "", err
		}
		return operation.Request().WorkspaceID, operation.Close()
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
