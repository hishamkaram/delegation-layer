package app

import (
	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func existingCommandResult(response Response, provider string, observed tasklifecycle.ExistingTaskResult) commandResult {
	if observed.RootID != "" {
		response.RootID, response.TaskID = observed.RootID, observed.TaskID
	}
	applyTaskObservation(&response, observed.Observation, "")
	result := existingPrimaryResult(response, provider, observed)
	applyEnvironmentRestoreError(&result, observed.EnvironmentRestoreError)
	mergeCommandClose(&result, func() error { return observed.TaskCloseError })
	if result.err != nil {
		result.err = admission.WithStage("existing-task", result.err)
		result.response.setError(result.err)
	}
	return result
}

func existingPrimaryResult(response Response, provider string, observed tasklifecycle.ExistingTaskResult) commandResult {
	if observed.Creation != nil {
		return creationCommandResult(response, provider, *observed.Creation)
	}
	if observed.Submission != nil {
		result := submissionCommandResult(response, *observed.Submission)
		if observed.Retried {
			return markDispatchSubmissionFailure(result)
		}
		return result
	}
	if observed.Failure == nil {
		return commandResult{response: response, code: 0}
	}
	code := classifyCode(observed.Failure, 1)
	switch observed.Stage {
	case tasklifecycle.ExistingPreparation:
	case tasklifecycle.ExistingProfile, tasklifecycle.ExistingRunnerRefresh:
		code = classifyCode(observed.Failure, 2)
	case tasklifecycle.ExistingExplicitRunner:
		code = 2
	case tasklifecycle.ExistingTerminalRelease, tasklifecycle.ExistingStarted, tasklifecycle.ExistingEnvironment:
		code = 1
	}
	return failed(response, observed.Failure, code)
}
