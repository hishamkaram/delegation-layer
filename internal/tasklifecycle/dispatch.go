package tasklifecycle

import (
	"context"
	"errors"
	"os"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func Dispatch(request DispatchRequest, options DispatchOptions) (result DispatchResult) {
	return DispatchContext(context.Background(), request, options)
}

func DispatchContext(ctx context.Context, request DispatchRequest, options DispatchOptions) (result DispatchResult) {
	result.Record = DispatchRecordLookup(request.TaskID, false, nil)
	result.TaskID = request.TaskID
	root, cwd, brief, err := dispatchInputs(request)
	if err != nil {
		return dispatchFailure(result, DispatchValidate, "validate-request", err)
	}
	result.Root = root
	if err = workspaces.RequireManagedRunner(WorkspaceDispatchRequest(request)); err != nil {
		return dispatchFailure(result, DispatchWorkspace, "prepare-workspace", err)
	}
	store, err := options.openStore(root, true)
	if err != nil {
		return dispatchFailure(result, DispatchOpenState, "open-state", err)
	}
	defer func() {
		result.StoreCleanup, result.StoreCloseError = closeAdmissionStore(store, result.Err())
	}()
	result.RootID = store.RootID
	id, err := dispatchTaskID(request)
	if err != nil {
		return dispatchFailure(result, DispatchGenerateID, "generate-task-id", err)
	}
	result.TaskID = id
	request, err = resolveDispatchTeamContext(ctx, request, options, store, id, cwd)
	if err != nil {
		return dispatchFailure(result, DispatchValidate, "resolve-team-member", err)
	}
	gate, resourceErr := openResourceGateContext(ctx, request, options, store, id)
	result.Resources = gate.current()
	if resourceErr != nil {
		return dispatchFailure(result, DispatchResources, "check-resources", resourceErr)
	}
	managed, managedCwd, workspaceErr := workspaces.PrepareDispatchContext(ctx, WorkspaceDispatchRequest(request), options.workspaceOptions(), store, id, cwd, brief)
	if managed != nil {
		defer func() {
			finished := managed.FinishContext(ctx, store, result.Err(), result.RecordPresence() == RecordCreated, admissioncore.RecoverInspectionLifetime)
			result.Workspace = &finished
		}()
	}
	if workspaceErr != nil {
		return dispatchFailure(result, DispatchWorkspace, "prepare-workspace", workspaceErr)
	}
	cwd = managedCwd
	if err = validatePreparedTeamProjectContext(ctx, request, options, cwd); err != nil {
		return dispatchFailure(result, DispatchWorkspace, "validate-team-project", err)
	}
	if managed != nil {
		request.WorkspaceBinding = managed.Binding()
	}
	return dispatchPreparedContext(ctx, request, options, store, cwd, brief, gate, result)
}

func dispatchPrepared(request DispatchRequest, options DispatchOptions, store *taskdir.Store, cwd string, brief []byte, gate *resourceGate, result DispatchResult) DispatchResult {
	return dispatchPreparedContext(context.Background(), request, options, store, cwd, brief, gate, result)
}

func dispatchPreparedContext(ctx context.Context, request DispatchRequest, options DispatchOptions, store *taskdir.Store, cwd string, brief []byte, gate *resourceGate, result DispatchResult) DispatchResult {
	var err error
	request, result, err = resolveDispatchProvider(request, options, store, cwd, brief, result)
	if err != nil {
		return result
	}
	result.Provider = request.Provider
	base, err := admissioncore.BuildRequest(store.RootID, result.TaskID, request.Provider, cwd, request.Config, brief, nil)
	if err != nil {
		return dispatchFailure(result, DispatchValidate, "validate-request", err)
	}
	base.Workspace = request.WorkspaceBinding
	base.Team = request.TeamContext.Clone()
	base.Remote = request.Remote.Clone()
	base.Origin = request.Origin.Clone()
	base.ConnectorContext = request.ConnectorContext.Clone()
	options, predecessorMeta, err := prepareDispatchContinuationContext(ctx, store, options, &base, request.ResumeTask)
	if err != nil {
		return dispatchFailure(result, DispatchPredecessor, "resolve-predecessor", err)
	}
	result.TaskID = base.TaskID
	td, openErr := store.OpenTask(result.TaskID)
	if openErr == nil {
		result.Record = RecordCreated
		if request.ResumeTask == "" {
			if err = rebaseExistingTeam(td, &base); err != nil {
				return dispatchFailure(result, DispatchOpenTask, "compare-team-request", errors.Join(err, td.Close()))
			}
		}
		result.Team = base.Team.Clone()
		existing := DispatchExistingContext(ctx, ExistingTaskOptions{Creation: options.Creation, Selection: request.Selection, RequestedBudget: request.Config.Budget, BriefPath: request.BriefPath}, store, td, base)
		result.Existing = &existing
		return result
	}
	if !errors.Is(openErr, os.ErrNotExist) {
		result.Record = RecordUnknown
		return dispatchFailure(result, DispatchOpenTask, "open-task-record", openErr)
	}
	result.Record = DispatchRecordLookup(request.TaskID, false, nil)
	creation, selection := newSuccessorCreation(options.Creation, request.Selection, store.Root, predecessorMeta)
	selection, err = continuationAdmissionSelection(selection, store.Root, predecessorMeta)
	if err != nil {
		return dispatchFailure(result, DispatchPredecessor, "resolve-continuation-supervisor", err)
	}
	created := createTaskContext(ctx, selection, creation, store, base, brief, request.BriefPath, gate)
	result.Creation = &created
	if created.TaskRecord == CreationRecordCreated {
		result.Team = base.Team.Clone()
	}
	result.Resources = gate.current()
	if request.ResumeTask == "" && base.Team != nil && created.Submission == nil && created.Stage == CreationRecord && teamCreationConflict(created.Err()) {
		return adoptCreatedTeamWinnerContext(ctx, request, options, store, base, result)
	}
	return result
}
