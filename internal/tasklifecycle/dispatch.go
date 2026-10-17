package tasklifecycle

import (
	"errors"
	"os"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func Dispatch(request DispatchRequest, options DispatchOptions) (result DispatchResult) {
	result.Record = DispatchRecordLookup(request.TaskID, false, nil)
	result.TaskID = request.TaskID
	root, cwd, brief, err := dispatchInputs(request)
	if err != nil {
		return dispatchFailure(result, DispatchValidate, "validate-request", err)
	}
	result.Root = root
	store, err := options.openStore(root, true)
	if err != nil {
		return dispatchFailure(result, DispatchOpenState, "open-state", err)
	}
	defer func() { result.StoreCloseError = store.Close() }()
	result.RootID = store.RootID
	id, err := dispatchTaskID(request)
	if err != nil {
		return dispatchFailure(result, DispatchGenerateID, "generate-task-id", err)
	}
	result.TaskID = id
	managed, managedCwd, workspaceErr := workspaces.PrepareDispatch(WorkspaceDispatchRequest(request), options.Workspace, store, id, cwd, brief)
	if managed != nil {
		defer func() {
			finished := managed.Finish(store, result.Err(), result.RecordPresence() == RecordCreated, admissioncore.RecoverInspectionLifetime)
			result.Workspace = &finished
		}()
	}
	if workspaceErr != nil {
		return dispatchFailure(result, DispatchWorkspace, "prepare-workspace", workspaceErr)
	}
	cwd = managedCwd
	if managed != nil {
		request.WorkspaceBinding = managed.Binding()
	}
	return dispatchPrepared(request, options, store, cwd, brief, result)
}

func dispatchPrepared(request DispatchRequest, options DispatchOptions, store *taskdir.Store, cwd string, brief []byte, result DispatchResult) (returned DispatchResult) {
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
	options, predecessorMeta, err := prepareDispatchContinuation(store, options, &base, request.ResumeTask)
	if err != nil {
		return dispatchFailure(result, DispatchPredecessor, "resolve-predecessor", err)
	}
	result.TaskID = base.TaskID
	td, openErr := store.OpenTask(result.TaskID)
	if openErr == nil {
		result.Record = RecordCreated
		existing := DispatchExisting(ExistingTaskOptions{Creation: options.Creation, Selection: request.Selection, RequestedBudget: request.Config.Budget, BriefPath: request.BriefPath, ApplyEnvironment: options.ApplyEnvironment}, store, td, base)
		result.Existing = &existing
		return result
	}
	if !errors.Is(openErr, os.ErrNotExist) {
		result.Record = RecordUnknown
		return dispatchFailure(result, DispatchOpenTask, "open-task-record", openErr)
	}
	result.Record = DispatchRecordLookup(request.TaskID, false, nil)
	restoreEnvironment, restoreErr := dispatchContinuationEnvironment(options.ApplyEnvironment, store.Root, predecessorMeta, base.RequestedConfig.Interactive)
	if restoreErr != nil {
		return dispatchFailure(result, DispatchEnvironment, "apply-continuation-environment", restoreErr)
	}
	defer func() {
		if restoreEnvironment != nil {
			returned.EnvironmentRestoreError = restoreEnvironment()
		}
	}()
	created := CreateTask(request.Selection, options.Creation, store, base, brief, request.BriefPath)
	result.Creation = &created
	return result
}
