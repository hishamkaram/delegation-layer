package app

import "github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

func dispatchCommandResult(observed tasklifecycle.DispatchResult) commandResult {
	response := newResponse("dispatch")
	response.RootID, response.TaskID = observed.RootID, observed.TaskID
	response.TaskRecord = taskRecordPresence(observed.Record)
	result := dispatchPrimaryResult(response, observed)
	applyEnvironmentRestoreError(&result, observed.EnvironmentRestoreError)
	if observed.Workspace != nil {
		applyManagedAdmissionFinish(observed.Root, *observed.Workspace, &result)
	}
	mergeCommandClose(&result, func() error { return observed.StoreCloseError })
	return result
}

func dispatchPrimaryResult(response Response, observed tasklifecycle.DispatchResult) commandResult {
	if observed.Existing != nil {
		return existingCommandResult(response, observed.Provider, *observed.Existing)
	}
	if observed.Creation != nil {
		return creationCommandResult(response, observed.Provider, *observed.Creation)
	}
	fallback := 1
	switch observed.Phase {
	case tasklifecycle.DispatchValidate, tasklifecycle.DispatchWorkspace, tasklifecycle.DispatchProviderSelection:
		fallback = 2
	case tasklifecycle.DispatchOpenState, tasklifecycle.DispatchProviderLookup, tasklifecycle.DispatchPredecessor, tasklifecycle.DispatchOpenTask:
	case tasklifecycle.DispatchGenerateID, tasklifecycle.DispatchEnvironment:
		return failed(response, observed.Failure, 1)
	}
	return failed(response, observed.Failure, classifyCode(observed.Failure, fallback))
}

func taskRecordPresence(record tasklifecycle.TaskRecordPresence) string {
	switch record {
	case tasklifecycle.RecordUnknown:
		return "unknown"
	case tasklifecycle.RecordAbsent:
		return "not_created"
	case tasklifecycle.RecordCreated:
		return "created"
	}
	return "unknown"
}

func dispatchTaskRecordLookup(requestedTaskID string, exists bool, lookupErr error) string {
	return taskRecordPresence(tasklifecycle.DispatchRecordLookup(requestedTaskID, exists, lookupErr))
}
