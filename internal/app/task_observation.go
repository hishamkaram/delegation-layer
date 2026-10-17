package app

import (
	"log"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"

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
	reportTaskMaintenanceDeferral(observed.MaintenanceError)
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
	observed, err := tasklifecycle.Collect(root, td, deps.collectionOptions(a.Watch))
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

func reportTaskMaintenanceDeferral(err error) {
	if err != nil {
		log.Printf("delegation-layer: task supervisor maintenance deferred: %v", err)
	}
}

func (deps storeDependencies) collectionOptions(watch time.Duration) tasklifecycle.CollectionOptions {
	var reported sync.Once
	return tasklifecycle.CollectionOptions{
		Watch: watch, ObserveSupervisor: deps.observeSupervisor,
		Supervisor: observationSupervisorOptions(deps.supervisorOptions, deps.initialSupervisorExecutable),
		Catalog:    deps.providerCatalog(), Registry: deps.registry,
		ReportMaintenanceDeferral: func(err error) {
			if err != nil {
				reported.Do(func() { reportTaskMaintenanceDeferral(err) })
			}
		},
	}
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
