package app

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

type workspaceBeforeSubmit = func(context.Context, *workspace.Operation) error

func scheduleWorkspaceOperation(observer context.Context, store *taskdir.Store, resource *workspace.Resource, deps Dependencies, spec workspacescore.OperationSpec, beforeSubmit workspaceBeforeSubmit) (*workspace.Operation, error) {
	return workspacescore.ScheduleOperation(observer, store, resource, workspaceRuntimeOptions(deps), spec, beforeSubmit)
}
