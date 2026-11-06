package workspaces

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func workspaceSubmissionClient(ctx context.Context, client *pueue.Client, kind string, deps OperationOptions) (context.Context, *pueue.Client, error) {
	if kind != workspace.OperationVerify {
		return ctx, client, nil
	}
	// Only the submitted verification job receives the caller environment.
	// Binding and cold daemon startup retain bounded control options.
	environment, err := deps.scopeEnvironment()
	if err != nil {
		return ctx, client, err
	}
	client, err = client.WithCommandEnvironment(environment)
	return ctx, client, err
}

// A committed receipt is historical evidence. Recheck current permission after
// its transaction returns, immediately before enqueue under the queue owner.
func workspaceOperationLaunchPermission(ctx context.Context, operation *workspace.Operation) error {
	if !time.Now().Before(operation.Deadline()) {
		return workspace.ErrPending
	}
	return LaunchPermissionError(operation.CheckLaunchPermission(ctx))
}
