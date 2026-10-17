package app

import (
	"time"

	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

type (
	WorkspaceResponse         = workspacescore.Description
	WorkspaceArtifactResponse = workspacescore.ArtifactDescription
)

func workspaceDispatchRequest(a Arguments) workspacescore.DispatchRequest {
	return tasklifecycle.WorkspaceDispatchRequest(dispatchRequest(a))
}

func prepareManagedDispatch(a Arguments, deps Dependencies, store *taskdir.Store, id, cwd string, brief []byte) (*workspacescore.Admission, string, error) {
	return workspacescore.PrepareDispatch(workspaceDispatchRequest(a), workspaceRuntimeOptions(deps), store, id, cwd, brief)
}

func prepareManagedContinuation(root string, deps Dependencies, predecessor task.TaskRecord) (*WorkspaceResponse, error) {
	return tasklifecycle.PrepareContinuationWorkspace(root, dispatchOptions(deps, false), predecessor)
}

func managedDispatchDigest(a Arguments, id, cwd string, brief []byte, budget time.Duration, resource *workspace.Resource) (string, error) {
	return workspacescore.DispatchDigest(workspaceDispatchRequest(a), id, cwd, brief, budget, resource)
}

func applyManagedAdmissionFinish(root string, finished workspacescore.AdmissionFinish, result *commandResult) {
	if finished.AbortError != nil {
		mergeCommandClose(result, func() error { return finished.AbortError })
	}
	result.response.Workspace = &finished.Description
	if finished.DescriptionError != nil {
		mergeCommandClose(result, func() error { return finished.DescriptionError })
	}
	setManagedAdmissionPendingFailure(root, finished.OperationID, finished.Description, result)
	mergeCommandClose(result, func() error { return finished.CloseError })
}
