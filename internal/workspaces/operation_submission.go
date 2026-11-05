package workspaces

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func workspaceSubmissionClient(ctx context.Context, store *taskdir.Store, client *pueue.Client, kind string, deps OperationOptions) (context.Context, *pueue.Client, error) {
	control, err := workspaceControlAdmissionContext(ctx, store, client, deps)
	if err != nil {
		return control, client, err
	}
	if kind != workspace.OperationVerify {
		return control, client, nil
	}
	// Only the submitted verification job receives the caller environment.
	// Binding and cold daemon startup retain bounded control options.
	environment, err := deps.scopeEnvironment()
	if err != nil {
		return control, client, err
	}
	client, err = client.WithCommandEnvironment(environment)
	return control, client, err
}

// A committed receipt is historical evidence. Recheck current permission after
// its transaction returns, immediately before enqueue under the queue owner.
func workspaceOperationLaunchPermission(ctx context.Context, store *taskdir.Store, operation *workspace.Operation, lease *taskdir.RootAdmissionLease) error {
	if !time.Now().Before(operation.Deadline()) {
		return workspace.ErrPending
	}
	if lease != nil {
		if err := lease.CheckRootContext(ctx, store); err != nil {
			return err
		}
	}
	return LaunchPermissionError(operation.CheckLaunchPermission(ctx))
}
