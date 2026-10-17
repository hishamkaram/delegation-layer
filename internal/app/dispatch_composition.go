package app

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func dispatchRequest(a Arguments) tasklifecycle.DispatchRequest {
	return tasklifecycle.DispatchRequest{
		Root: a.Root, TaskID: a.TaskID, GeneratedTaskID: a.generatedTaskID, ResumeTask: a.ResumeTask,
		Provider: a.Provider, Auto: a.Auto, Config: a.Config, Cwd: a.Cwd, BriefPath: a.Brief, BriefBytes: a.BriefBytes,
		Isolation: a.Isolation, WorkspaceID: a.WorkspaceID, Base: a.Base, WorkspaceBudget: a.WorkspaceBudget,
		WorkspaceBinding: a.workspaceBinding, Selection: supervisorAdmissionSelection(a),
	}
}

func dispatchOptions(deps Dependencies, continuation bool) tasklifecycle.DispatchOptions {
	creationDeps := deps
	if continuation {
		creationDeps = deps.normalized()
	}
	return tasklifecycle.DispatchOptions{
		Creation: tasklifecycle.CreationOptions{Admission: admissionPreparationOptions(creationDeps), PublisherVersion: creationDeps.PublisherVersion, OpenValidationStore: submissionValidationStore(creationDeps)},
		Catalog:  deps.normalized().Catalog, Workspace: workspaceRuntimeOptions(deps),
		OpenStore: func(root string, create bool) (*taskdir.Store, error) {
			return openStore(root, deps.storeDependencies(), create)
		},
		ContinuationProfiles: func(meta task.MetaRecord) admissioncore.Profiles {
			return admissionProfiles(continuationDependencies(deps, meta))
		},
		ApplyEnvironment: applySavedEnvironment,
	}
}

func dispatch(a Arguments, deps Dependencies) commandResult {
	return dispatchCommandResult(tasklifecycle.Dispatch(dispatchRequest(a), dispatchOptions(deps, a.ResumeTask != "")))
}

func continueTask(a Arguments, deps Dependencies) commandResult {
	options := tasklifecycle.ContinuationOptions{Dispatch: dispatchOptions(deps, true), Supervisor: continuationSupervisorOptions(deps.normalized())}
	return continuationCommandResult(tasklifecycle.Continue(dispatchRequest(a), options))
}
