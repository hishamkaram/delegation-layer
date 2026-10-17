package app

import (
	"log"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func status(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("status")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStore(root, deps.storeDependencies(), false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	defer func() { fillManagedTaskWorkspace(store, td, &result) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	observed, err := tasklifecycle.Status(store, td, observationSupervisorOptions(deps.SupervisorOptions, deps.InitialSupervisorExecutable), deps.RunnerExecutable, deps.storeDependencies().providerCatalog())
	if observed.MaintenanceError != nil {
		log.Printf("delegation-layer: task supervisor maintenance deferred: %v", observed.MaintenanceError)
	}
	applyTaskObservation(&response, observed.Observation, root)
	if observed.ControlObservation {
		return statusObservationResult(response, err)
	}
	if err != nil {
		return failed(response, err, 1)
	}
	return commandResult{response: response, code: 0}
}

func collect(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("collect")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStore(root, deps, false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	defer func() { fillManagedTaskWorkspace(store, td, &result) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	observed, err := tasklifecycle.Collect(root, td, tasklifecycle.CollectionOptions{
		Watch: a.Watch, ObserveSupervisor: deps.observeSupervisor,
		Supervisor: observationSupervisorOptions(deps.supervisorOptions, deps.initialSupervisorExecutable),
		Catalog:    deps.providerCatalog(), Registry: deps.registry,
	})
	applyTaskObservation(&response, observed.Observation, root)
	response.Raw = observed.Raw
	if observed.Completion != nil {
		return completedCollectionResponse(response, observed.Completion, root, err)
	}
	if err != nil {
		return failed(response, err, 1)
	}
	if observed.Waiting {
		return commandResult{response: response, code: 3}
	}
	return commandResult{response: response, code: 0}
}

func completedCollectionResponse(response Response, completion *tasklifecycle.CollectionCompletion, root string, finalErr error) commandResult {
	result := collectionResult(response, completion.Outcome, completion.CollectionError)
	applyContinuationOffer(&result.response, root, completion.Continuation)
	if completion.ReleaseError != nil {
		result.err = finalErr
		result.code = 1
		result.response.setError(finalErr)
	}
	applyRejectedRecovery(&result.response, completion.RecoveryAvailable)
	return result
}

func statusObservationResult(response Response, err error) commandResult {
	if err != nil && !tasklifecycle.IsPureSupervisorUncertaintyError(err) {
		return failed(response, err, 1)
	}
	return commandResult{response: response, code: 0, err: err}
}

func collectionResult(response Response, outcome *task.OutcomeRecord, collectErr error) commandResult {
	response.Outcome = outcome
	response.Payload = &outcome.Payload
	response.EvidenceSHA256 = outcome.EvidenceSHA256
	applyOutputDiagnostics(&response, outcome.Diagnostics)
	if outcome.Verdict == task.VerdictCommitted {
		response.Publication = task.PublicationCommitted.String()
		if collectErr != nil {
			return failed(response, collectErr, 1)
		}
		return commandResult{response: response, code: 0}
	}
	response.Publication = task.PublicationRejected.String()
	if collectErr != nil {
		return failed(response, collectErr, 1)
	}
	return commandResult{response: response, code: 4}
}

func collectionInspectionResult(response Response, inspection *taskdir.TaskInspection, collectErr, inspectErr, descriptorErr error) (commandResult, bool) {
	outcome, err, handled := tasklifecycle.CollectionInspectionOutcome(inspection, collectErr, inspectErr, descriptorErr)
	if !handled {
		return commandResult{}, false
	}
	return collectionResult(response, outcome, err), true
}
