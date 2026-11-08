package app

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func beginWorkspaceForget(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, options workspacescore.OperationOptions, budget time.Duration) (ack workspacescore.ActionAcknowledgement, resultErr error) {
	return workspacescore.BeginForget(ctx, store, resource, options, budget)
}
