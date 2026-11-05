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
	deps := Dependencies{RunnerExecutable: meta.RunnerExecutable, Environment: environment}.normalized()
	options := workspaceRuntimeOptions(deps)
	resources := &workspaceCommandResources{store: store}
	defer func() { resultErr = errors.Join(resultErr, resources.closeAfter(resultErr)) }()
	ctx, lease, err := resources.acquire(ctx, options.RootAdmission, deps.SupervisorOptions)
	if err != nil {
		return err
	}
	options.RootAdmission = lease
	resource, err := workspace.LoadResource(ctx, store, request.Workspace.ID)
	resources.resource = resource
	if err != nil {
		return err
	}
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
	return reconcileWorkspaceUse(ctx, store, resource, options)
}
