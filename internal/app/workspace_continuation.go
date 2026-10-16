package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func managedContinuationFailure(root string, response Response, cause error) commandResult {
	result := failed(response, withDispatchStage("prepare-workspace", cause), 1)
	if response.Workspace != nil {
		setManagedAdmissionPendingFailure(root, "", *response.Workspace, &result)
	}
	return result
}

// prepareManagedContinuation checks resource availability before provider
// preparation can read a deleted or still-busy checkout. Dispatch repeats the
// reservation check under its own owner lease before creating the successor.
func prepareManagedContinuation(root string, deps Dependencies, predecessor task.TaskRecord) (response *WorkspaceResponse, resultErr error) {
	if predecessor.Workspace == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	store, err := openStore(root, deps.storeDependencies(), false)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	resource, err := workspace.LoadResource(ctx, store, predecessor.Workspace.ID)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	owner, err := resource.TryAcquireUseOwner(ctx)
	if errors.Is(err, task.ErrLockBusy) {
		return &WorkspaceResponse{ID: resource.Request().WorkspaceID, Isolation: "git-worktree", State: "busy", EvidenceState: "unknown", NextAction: "watch_workspace"}, workspace.ErrBusy
	}
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Release()) }()
	availabilityErr := managedContinuationAvailable(ctx, store, resource, deps, predecessor)
	description, describeErr := describeWorkspace(ctx, resource)
	return &description, errors.Join(availabilityErr, describeErr)
}

func managedContinuationAvailable(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, predecessor task.TaskRecord) error {
	if _, err := resource.Release(ctx); err == nil {
		return workspace.ErrReleased
	} else if !workspace.OperationAbsent(err) {
		return err
	}
	if err := reconcileWorkspaceUse(ctx, store, resource, deps); err != nil {
		return err
	}
	checkout, err := registeredExecutionCheckout(ctx, resource)
	if err != nil {
		return err
	}
	if checkout.Workdir != predecessor.CanonicalCwd {
		return task.ErrIdentityMismatch
	}
	return nil
}
