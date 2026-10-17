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

func argumentsFromDispatch(a Arguments, request tasklifecycle.DispatchRequest) Arguments {
	a.Root, a.TaskID, a.generatedTaskID, a.ResumeTask = request.Root, request.TaskID, request.GeneratedTaskID, request.ResumeTask
	a.Provider, a.Auto, a.Config, a.Cwd = request.Provider, request.Auto, request.Config, request.Cwd
	a.Brief, a.BriefBytes = request.BriefPath, request.BriefBytes
	a.Isolation, a.WorkspaceID, a.Base, a.WorkspaceBudget = request.Isolation, request.WorkspaceID, request.Base, request.WorkspaceBudget
	a.workspaceBinding = request.WorkspaceBinding
	a.Runner, a.runnerOwnership, a.PueueConfig, a.savedSupervisor = request.Selection.Runner, request.Selection.RunnerOwnership, request.Selection.ConfigPath, request.Selection.SavedSupervisor
	return a
}
