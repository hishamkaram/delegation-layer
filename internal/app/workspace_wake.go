package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// wakeManagedTaskWorkspace resumes only the capture obligation recorded before
// submission. It never creates or retries provider work. The nonblocking use
// lease prevents queue-maintenance completion from waiting on its own caller.
func wakeManagedTaskWorkspace(ctx context.Context, store *taskdir.Store, td *taskdir.TaskDir) (resultErr error) {
	request, meta, err := td.PreparedRecords()
	if err != nil {
		return err
	}
	if request.Workspace == nil {
		return nil
	}
	resource, err := workspace.LoadResource(ctx, store, request.Workspace.ID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	owner, err := resource.TryAcquireUseOwner(ctx)
	if errors.Is(err, task.ErrLockBusy) {
		return workspace.ErrPending
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Release()) }()
	return reconcileWorkspaceUse(ctx, store, resource, Dependencies{RunnerExecutable: meta.RunnerExecutable}.normalized())
}
