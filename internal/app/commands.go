package app

import (
	"errors"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func loadContinuationInput(root string, a Arguments, deps Dependencies) (*task.TaskRecord, *task.MetaRecord, []byte, error) {
	return tasklifecycle.LoadContinuationInput(root, dispatchRequest(a), dispatchOptions(deps, false))
}

func buildContinuationArguments(a Arguments, root string, predecessor *task.TaskRecord, meta *task.MetaRecord, brief []byte) (Arguments, error) {
	request, err := tasklifecycle.BuildContinuationRequest(dispatchRequest(a), root, predecessor, meta, brief)
	if err != nil {
		return Arguments{}, err
	}
	result := argumentsFromDispatch(a, request)
	result.Command = "dispatch"
	return result, nil
}

// continuationDependencies keeps the recorded provider preparation contract
// attached to every candidate compiled for this successor. The production
// catalog owns the historical interpretation; injected profile hooks retain
// their test and acceptance behavior.
func continuationDependencies(deps Dependencies, predecessorMeta task.MetaRecord) Dependencies {
	normalized := deps.normalized()
	if !normalized.useCatalogExisting {
		return normalized
	}
	normalized.PrepareCandidate = func(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
		if request.RequestedConfig.Interactive && request.PriorSession != nil {
			return normalized.Catalog.Candidate(request)
		}
		return normalized.Catalog.ExistingCandidate(request, predecessorMeta)
	}
	return normalized
}

func continuationDependenciesForTask(store *taskdir.Store, deps Dependencies, id string) (Dependencies, *task.MetaRecord, error) {
	_, meta, err := tasklifecycle.ContinuationOptionsForTask(store, dispatchOptions(deps, true), id)
	if err != nil {
		return deps, meta, err
	}
	return continuationDependencies(deps, *meta), meta, nil
}

func validateContinuationMetadata(root string, a Arguments, meta *task.MetaRecord) error {
	return tasklifecycle.ValidateContinuationMetadata(root, supervisorAdmissionSelection(a), meta)
}

func applyContinuationEnvironment(root string, meta *task.MetaRecord) (func() error, error) {
	return supervisorcore.ApplyContinuationEnvironment(root, meta, applySavedEnvironment)
}

func decorateContinuationResult(result commandResult, root, predecessorID string, mode commonprovider.ContinuationMode, runner string, supervisor *task.SupervisorRef) commandResult {
	result.response.Command = "continue"
	result.response.ParentTaskID = predecessorID
	result.response.Continuation = &ContinuationResponse{Resumable: result.err == nil, Mode: string(mode), PredecessorID: predecessorID}
	if result.err == nil {
		result.response.Continuation.ContinueCommand = continuationCommand(root, result.response.TaskID, runner, supervisor)
	}
	return result
}

func dispatchExisting(a Arguments, deps Dependencies, store *taskdir.Store, td *taskdir.TaskDir, req task.TaskRecord, response Response) commandResult {
	options := tasklifecycle.ExistingTaskOptions{Creation: tasklifecycle.CreationOptions{Admission: admissionPreparationOptions(deps), PublisherVersion: deps.PublisherVersion, OpenValidationStore: submissionValidationStore(deps)}, Selection: supervisorAdmissionSelection(a), RequestedBudget: a.Config.Budget, BriefPath: a.Brief, ApplyEnvironment: applySavedEnvironment}
	return existingCommandResult(response, req.Provider, tasklifecycle.DispatchExisting(options, store, td, req))
}

func markDispatchSubmissionFailure(result commandResult) commandResult {
	if result.err != nil {
		result.err = tasklifecycle.SubmissionFailure(result.err, result.response.Admission == task.AdmissionAdmitted.String())
		result.response.setError(result.err)
	}
	return result
}

func stopResponses(records []taskdir.StopRecord) []StopResponse {
	responses := make([]StopResponse, 0, len(records))
	for _, record := range records {
		responses = append(responses, describeStopRecord(record))
	}
	return responses
}

func failed(response Response, err error, code int) commandResult {
	if errors.Is(err, commonprovider.ErrAuthenticationBlocked) {
		response.Status = preflightStatusBlocked
	}
	response.setError(err)
	return commandResult{response: response, code: code, err: err}
}

func resolvePredecessor(store *taskdir.Store, deps Dependencies, request task.TaskRecord, id string) (*task.PriorSession, error) {
	normalized := deps.normalized()
	options := tasklifecycle.PredecessorOptions{Admission: staticAdmissionOptions(normalized), Supervisor: supervisorAdmissionOptions(normalized, normalized.SupervisorOptions), ApplyEnvironment: applySavedEnvironment}
	return tasklifecycle.ResolvePredecessor(store, options, request, id)
}

func predecessorTerminated(root string, td *taskdir.TaskDir, req *task.TaskRecord, supervisorOptions pueue.Options, initialSupervisorExecutable string) error {
	return tasklifecycle.PredecessorTerminated(root, td, req, observationSupervisorOptions(supervisorOptions, initialSupervisorExecutable))
}
