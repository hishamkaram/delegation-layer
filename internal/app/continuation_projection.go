package app

import "github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

func continuationCommandResult(observed tasklifecycle.ContinuationResult) commandResult {
	if observed.Dispatch != nil {
		result := dispatchCommandResult(*observed.Dispatch)
		result = decorateContinuationResult(result, observed.Root, observed.PredecessorID, observed.Mode, observed.Runner, observed.Supervisor)
		applyEnvironmentRestoreError(&result, observed.EnvironmentRestoreError)
		return result
	}
	response := newResponse("continue")
	response.TaskRecord = "not_created"
	response.RootID, response.Workspace = observed.RootID, observed.Workspace
	code := 1
	switch observed.Phase {
	case tasklifecycle.ContinueProvider, tasklifecycle.ContinueValidation, tasklifecycle.ContinueSession:
		code = 2
	case tasklifecycle.ContinueWorkspace:
		return managedContinuationFailure(observed.Root, response, observed.Failure)
	case tasklifecycle.ContinueResolveRoot, tasklifecycle.ContinueLoadPredecessor, tasklifecycle.ContinueRunner, tasklifecycle.ContinueGenerateID, tasklifecycle.ContinueSupervisor, tasklifecycle.ContinueEnvironment:
	}
	return failed(response, observed.Failure, code)
}
