package tasklifecycle

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func PrepareContinuationWorkspace(root string, options DispatchOptions, predecessor task.TaskRecord) (response *workspacescore.Description, resultErr error) {
	if predecessor.Workspace == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	store, err := options.openStore(root, false)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	return workspacescore.PrepareContinuation(ctx, store, predecessor, options.Workspace)
}
