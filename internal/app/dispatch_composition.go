package app

import (
	"context"
	"errors"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	teamcontract "github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

func dispatchRequest(a Arguments, deps Dependencies) tasklifecycle.DispatchRequest {
	return tasklifecycle.DispatchRequest{
		ExpectedTeamSelection: a.ExpectedTeamSelection,
		Origin:                a.Connector.origin, ConnectorContext: a.Connector.binding,
		Remote: a.remoteBinding, Root: a.Root, TaskID: a.TaskID, GeneratedTaskID: a.generatedTaskID, ResumeTask: a.ResumeTask,
		Provider: a.Provider, Auto: a.Auto, Config: a.Config, Cwd: a.Cwd, BriefPath: a.Brief, BriefBytes: a.BriefBytes,
		Isolation: a.Isolation, WorkspaceID: a.WorkspaceID, Base: a.Base, WorkspaceBudget: a.WorkspaceBudget,
		WorkspaceBinding: a.workspaceBinding, Selection: supervisorAdmissionSelection(a, deps),
		TeamTarget: requestedTeamTarget(a), TeamContext: a.teamContext.Clone(), ProjectRoot: a.ProjectRoot,
	}
}

func dispatchOptions(deps Dependencies, continuation bool) tasklifecycle.DispatchOptions {
	return dispatchOptionsContext(context.Background(), deps, continuation)
}

func dispatchOptionsContext(ctx context.Context, deps Dependencies, continuation bool) tasklifecycle.DispatchOptions {
	creationDeps := deps
	if continuation {
		creationDeps = deps.normalized()
	}
	return tasklifecycle.DispatchOptions{
		Creation: tasklifecycle.CreationOptions{Admission: admissionPreparationOptions(creationDeps), PublisherVersion: creationDeps.PublisherVersion, OpenValidationStore: submissionValidationStoreContext(ctx, creationDeps)},
		Catalog:  deps.normalized().Catalog, Workspace: workspaceRuntimeOptions(deps),
		OpenStore: func(root string, create bool) (*taskdir.Store, error) {
			return openStoreContext(ctx, root, deps.storeDependencies(), create)
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
	return dispatchContext(context.Background(), a, deps)
}

func dispatchContext(ctx context.Context, a Arguments, deps Dependencies) (result commandResult) {
	defer func() { result.connectorOutput = a.Connector.outputPolicy }()
	if err := prepareGuardedDispatchContext(ctx, &a, deps); err != nil {
		return connectorFailure(newResponse("dispatch"), err)
	}
	deps = deps.withCapturedEnvironmentContext(ctx)
	options := dispatchOptionsContext(ctx, deps, a.ResumeTask != "")
	options.ResourceGuards = a.ResourceGuards
	if a.Connector.ExpectedRoot != "" {
		options.OpenStore = func(root string, _ bool) (*taskdir.Store, error) {
			store, err := openStoreContext(ctx, root, deps.storeDependencies(), false)
			if err != nil {
				return nil, err
			}
			if _, err = store.RequireWorkspaceConnectorContext(ctx, a.Connector.ExpectedRoot, a.Connector.ExpectedProject, a.Cwd, connectorWorkspace(a.Connector)); err != nil {
				return nil, errors.Join(err, store.Close())
			}
			return store, nil
		}
	}
	result = dispatchCommandResult(tasklifecycle.DispatchContext(ctx, dispatchRequest(a, deps), options))
	return attachConnectorDispatchReceipt(ctx, a, deps, result)
}

func continueTask(a Arguments, deps Dependencies) commandResult {
	return continueTaskContext(context.Background(), a, deps)
}

func continueTaskContext(ctx context.Context, a Arguments, deps Dependencies) (result commandResult) {
	defer func() { result.connectorOutput = a.Connector.outputPolicy }()
	if hasConnectorGuards(a.Connector) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Connector.ControlTimeout)
		defer cancel()
	}
	deps = deps.withCapturedEnvironmentContext(ctx)
	options := tasklifecycle.ContinuationOptions{Dispatch: dispatchOptionsContext(ctx, deps, true), SuccessorTaskID: a.SuccessorTaskID}
	options.Dispatch.ResourceGuards = a.ResourceGuards
	if hasConnectorGuards(a.Connector) {
		if err := loadConnectorOrigin(ctx, &a, deps); err != nil {
			return connectorFailure(newResponse("continue"), err)
		}
		guard := connectorTaskSelection(a)
		if err := guard.Validate(); err != nil {
			return connectorFailure(newResponse("continue"), err)
		}
		options.PredecessorGuard = &guard
		var origin task.GatewayOrigin
		if err := readConnectorJSON(ctx, a.Connector.SuccessorOriginFile, deps.Input, &origin); err != nil {
			return connectorFailure(newResponse("continue"), err)
		}
		if err := task.ValidateGatewayOrigin(origin); err != nil {
			return connectorFailure(newResponse("continue"), err)
		}
		a.Connector.origin = &origin
		a.Connector.outputPolicy = a.Connector.outputPolicy.withOrigin(origin)
	}
	result = continuationCommandResult(tasklifecycle.ContinueContext(ctx, dispatchRequest(a, deps), options))
	if hasConnectorGuards(a.Connector) {
		a.TaskID = a.SuccessorTaskID
		a.Connector.ExpectedSpecSHA256 = ""
		result = attachConnectorDispatchReceipt(ctx, a, deps, result)
	}
	return result
}
