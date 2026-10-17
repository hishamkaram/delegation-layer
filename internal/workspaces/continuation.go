package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// PrepareContinuation checks resource availability before provider
// preparation can read a deleted or still-busy checkout. Dispatch repeats the
// reservation check under its own owner lease before creating the successor.
func PrepareContinuation(ctx context.Context, store *taskdir.Store, predecessor task.TaskRecord, deps OperationOptions) (response *Description, resultErr error) {
	resource, err := workspace.LoadResource(ctx, store, predecessor.Workspace.ID)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	owner, err := resource.TryAcquireUseOwner(ctx)
	if errors.Is(err, task.ErrLockBusy) {
		return &Description{ID: resource.Request().WorkspaceID, Isolation: "git-worktree", State: "busy", EvidenceState: "unknown", NextAction: "watch_workspace"}, workspace.ErrBusy
	}
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Release()) }()
	availabilityErr := continuationAvailable(ctx, store, resource, deps, predecessor)
	description, describeErr := Describe(ctx, resource)
	return &description, errors.Join(availabilityErr, describeErr)
}

func continuationAvailable(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps OperationOptions, predecessor task.TaskRecord) error {
	if _, err := resource.Release(ctx); err == nil {
		return workspace.ErrReleased
	} else if !workspace.OperationAbsent(err) {
		return err
	}
	if err := ReconcileUse(ctx, store, resource, deps); err != nil {
		return err
	}
	checkout, err := RegisteredExecutionCheckout(ctx, resource)
	if err != nil {
		return err
	}
	if checkout.Workdir != predecessor.CanonicalCwd {
		return task.ErrIdentityMismatch
	}
	return nil
}
