package tasklifecycle

import (
	"context"
	"errors"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func PrepareContinuationWorkspace(root string, options DispatchOptions, predecessor task.TaskRecord) (response *workspacescore.Description, resultErr error) {
	return PrepareContinuationWorkspaceContext(context.Background(), root, options, predecessor)
}

func PrepareContinuationWorkspaceContext(ctx context.Context, root string, options DispatchOptions, predecessor task.TaskRecord) (response *workspacescore.Description, resultErr error) {
	if predecessor.Workspace == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, workspace.DefaultBudget)
	defer cancel()
	store, err := options.openStore(root, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !supervisorcore.CleanupRetainsStore(resultErr, store) {
			resultErr = errors.Join(resultErr, store.Close())
		}
	}()
	deps := options.Workspace
	deps.RootAdmission = options.Creation.RootAdmission
	return workspacescore.PrepareContinuation(ctx, store, predecessor, deps)
}
