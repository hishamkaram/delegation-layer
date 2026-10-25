package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// wakeManagedTaskWorkspace resumes only the capture obligation recorded before
// submission. It never creates or retries provider work. The nonblocking use
// lease prevents queue-maintenance completion from waiting on its own caller.
// environment is the observing command's captured launch scope; auxiliary
// helpers it submits run there. Nil is a missing capture, not the process
// environment; a nonnil empty scope is explicit and kept.
func wakeManagedTaskWorkspace(ctx context.Context, store *taskdir.Store, td *taskdir.TaskDir, environment []string) (resultErr error) {
	request, meta, err := td.PreparedRecords()
	if err != nil {
		return err
	}
	if request.Workspace == nil {
		return nil
	}
	if environment == nil {
		return fmt.Errorf("%w: workspace wake launch environment was not captured", task.ErrEvidenceFault)
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
	return reconcileWorkspaceUse(ctx, store, resource, Dependencies{RunnerExecutable: meta.RunnerExecutable, Environment: environment}.normalized())
}
