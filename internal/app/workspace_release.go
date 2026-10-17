package app

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func beginWorkspaceRelease(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, discard bool, budget time.Duration) (ack workspacescore.ActionAcknowledgement, resultErr error) {
	return workspacescore.BeginRelease(ctx, store, resource, workspaceRuntimeOptions(deps), discard, budget)
}
