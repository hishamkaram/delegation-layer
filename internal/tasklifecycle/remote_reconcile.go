package tasklifecycle

import (
	"context"
	"errors"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ReconcileRemoteContext only recovers an existing matching immutable request.
// It never provisions a workspace, selects a replacement, or creates absent work.
func ReconcileRemoteContext(ctx context.Context, request DispatchRequest, options DispatchOptions) (result DispatchResult) {
	result.Record = RecordUnknown
	result.TaskID = request.TaskID
	root, cwd, brief, err := dispatchInputs(request)
	if err != nil {
		return dispatchFailure(result, DispatchValidate, "validate-request", err)
	}
	result.Root = root
	store, err := options.openStore(root, false)
	if err != nil {
		return dispatchFailure(result, DispatchOpenState, "open-state", err)
	}
	defer func() { result.StoreCleanup, result.StoreCloseError = closeAdmissionStore(store, result.Err()) }()
	result.RootID = store.RootID
	td, err := store.OpenTask(request.TaskID)
	if err != nil {
		return dispatchFailure(result, DispatchOpenTask, "open-existing-remote-task", err)
	}
	saved, err := td.RecordedRequest()
	if err != nil {
		return dispatchFailure(result, DispatchOpenTask, "validate-remote-receipt", errors.Join(err, td.Close()))
	}
	candidate, err := admissioncore.BuildRequest(store.RootID, request.TaskID, request.Provider, cwd, request.Config, brief, nil)
	if err != nil {
		return dispatchFailure(result, DispatchValidate, "validate-request", errors.Join(err, td.Close()))
	}
	candidate.Remote = request.Remote.Clone()
	if saved.Remote == nil || !task.CompareRequests(&candidate, saved) {
		return dispatchFailure(result, DispatchValidate, "compare-remote-request", errors.Join(task.ErrRequestConflict, td.Close()))
	}
	result.Record = RecordCreated
	existing := DispatchExistingContext(ctx, ExistingTaskOptions{Creation: options.Creation, Selection: request.Selection, RequestedBudget: request.Config.Budget, BriefPath: request.BriefPath}, store, td, candidate)
	result.Existing = &existing
	return result
}
