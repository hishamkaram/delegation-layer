package app

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func reconcileWorkspaceForgetOperation(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, retry bool, budget time.Duration) (operationID string, handled bool, resultErr error) {
	return workspacescore.ReconcileForgetOperation(ctx, store, resource, workspaceRuntimeOptions(deps), retry, budget)
}

func reconcileWorkspaceRemoval(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, retry bool) (resultErr error) {
	return workspacescore.ReconcileRemoval(ctx, store, resource, workspaceRuntimeOptions(deps), retry)
}

func scheduleExistingWorkspaceOperation(observer context.Context, store *taskdir.Store, operation *workspace.Operation, deps Dependencies) error {
	return workspacescore.ScheduleExistingOperation(observer, store, operation, workspaceRuntimeOptions(deps))
}

func submitWorkspaceOperation(ctx context.Context, store *taskdir.Store, operation *workspace.Operation, deps Dependencies) (resultErr error) {
	return workspacescore.SubmitOperation(ctx, store, operation, workspaceRuntimeOptions(deps))
}
