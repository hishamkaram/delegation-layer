package app

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

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

func decorateContinuationResult(result commandResult, root, predecessorID string, mode commonprovider.ContinuationMode, runner string, supervisor *task.SupervisorRef) commandResult {
	result.response.Command = "continue"
	result.response.ParentTaskID = predecessorID
	result.response.Continuation = &ContinuationResponse{Resumable: result.err == nil, Mode: string(mode), PredecessorID: predecessorID}
	if result.err == nil {
		result.response.Continuation.ContinueCommand = continuationCommand(root, result.response.TaskID, runner, supervisor)
	}
	return result
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
