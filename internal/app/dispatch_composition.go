package app

import (
	"context"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	teamcontract "github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

func dispatchRequest(a Arguments, deps Dependencies) tasklifecycle.DispatchRequest {
	return tasklifecycle.DispatchRequest{
		Root: a.Root, TaskID: a.TaskID, GeneratedTaskID: a.generatedTaskID, ResumeTask: a.ResumeTask,
		Provider: a.Provider, Auto: a.Auto, Config: a.Config, Cwd: a.Cwd, BriefPath: a.Brief, BriefBytes: a.BriefBytes,
		Isolation: a.Isolation, WorkspaceID: a.WorkspaceID, Base: a.Base, WorkspaceBudget: a.WorkspaceBudget,
		WorkspaceBinding: a.workspaceBinding, Selection: supervisorAdmissionSelection(a, deps),
		TeamTarget: requestedTeamTarget(a), TeamContext: a.teamContext.Clone(), ProjectRoot: a.ProjectRoot,
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
		ContinuationProfiles: func(meta task.MetaRecord, environment []string) admissioncore.Profiles {
			return profilesForEnvironment(continuationDependencies(deps, meta), environment)
		},
		Resources: deps.ResourceChecker,
		Members: func(ctx context.Context, target task.TeamTarget, root, cwd, projectRoot string) (teamcontract.ResolvedMember, error) {
			return lookupTeamMember(ctx, target, root, cwd, projectRoot, deps)
		},
		CheckTeamProject: func(ctx context.Context, reference task.TeamReference, cwd, projectRoot string) error {
			return checkTeamProject(ctx, reference, cwd, projectRoot, deps)
		},
	}
}

func dispatch(a Arguments, deps Dependencies) commandResult {
	deps = deps.withCapturedEnvironment()
	return dispatchCommandResult(tasklifecycle.Dispatch(dispatchRequest(a, deps), dispatchOptions(deps, a.ResumeTask != "")))
}

func continueTask(a Arguments, deps Dependencies) commandResult {
	deps = deps.withCapturedEnvironment()
	options := tasklifecycle.ContinuationOptions{Dispatch: dispatchOptions(deps, true)}
	return continuationCommandResult(tasklifecycle.Continue(dispatchRequest(a, deps), options))
}
