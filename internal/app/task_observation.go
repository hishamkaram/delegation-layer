package app

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"

	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func status(a Arguments, deps Dependencies) commandResult {
	return statusContext(context.Background(), a, deps)
}

func statusContext(ctx context.Context, a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("status")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStoreContext(ctx, root, deps.storeDependencies(), false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { closeObservationStore(&result, store) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	// Runs before td.Close; it never changes the command result.
	defer func() {
		if !cleanupRetainsStore(result.err, store) {
			reportContextTaskMaintenance(ctx, taskMaintenanceObserver{root: root, options: deps.supervisorControlScope(), lookup: deps.supervisorLookup()}, td)
		}
	}()
	defer func() { fillRecordedTaskContextForOperation(ctx, store, td, deps.Environment, &result) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	observed, err := tasklifecycle.StatusContext(ctx, store, td, observationSupervisorOptions(deps.supervisorControlScope(), deps.supervisorLookup(), deps.infrastructureRouting()), deps.RunnerExecutable, deps.storeDependencies().providerCatalog())
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

func collect(a Arguments, deps storeDependencies) commandResult {
	return collectContext(context.Background(), a, deps)
}

func collectContext(ctx context.Context, a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("collect")
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStoreContext(ctx, root, deps, false)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { closeObservationStore(&result, store) }()
	td, err := store.OpenTask(a.TaskID)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, td.Close) }()
	// Runs before td.Close; it never changes the command result.
	defer func() {
		if !cleanupRetainsStore(result.err, store) {
			reportContextTaskMaintenance(ctx, taskMaintenanceObserver{root: root, options: deps.supervisorOptions, lookup: deps.supervisorLookup}, td)
		}
	}()
	defer func() { fillRecordedTaskContextForOperation(ctx, store, td, deps.environment, &result) }()
	response.RootID, response.TaskID = store.RootID, a.TaskID
	observed, err := tasklifecycle.CollectContext(ctx, root, td, deps.collectionOptions(a.Watch))
	applyTaskObservation(&response, observed.Observation, root)
	response.Raw = observed.Raw
	if observed.Completion != nil {
		return completedCollectionResponse(response, observed.Completion, root, err)
	}
	if err != nil {
		return failed(response, err, 1)
	}
	if observed.Interrupted() {
		return failed(response, errTaskInterrupted, 1)
	}
	if observed.Waiting {
		return commandResult{response: response, code: 3}
	}
	return commandResult{response: response, code: 0}
}

// errTaskInterrupted is collect's operational failure for a turn whose native
// lifetime closed without a seal or outcome. Status reports the same
// observation successfully because observing it is not a failure.
var errTaskInterrupted = errors.New("task was interrupted before it published an outcome")

func reportTaskMaintenanceDeferral(err error) {
	if err != nil {
		log.Printf("delegation-layer: task supervisor maintenance deferred: %v", err)
	}
}

func (deps storeDependencies) collectionOptions(watch time.Duration) tasklifecycle.CollectionOptions {
	var reported sync.Once
	return tasklifecycle.CollectionOptions{
		Watch: watch, ObserveSupervisor: deps.observeSupervisor,
		Supervisor: observationSupervisorOptions(deps.supervisorOptions, deps.supervisorLookup, deps.infrastructureRouting()),
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

// A completed command's maintenance wake has its own existing finite lifetime.
func reportContextTaskMaintenance(parent context.Context, observer taskMaintenanceObserver, td *taskdir.TaskDir) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), lifetimeCustodyReleaseTimeout)
	defer cancel()
	if err := observer.observeTask(ctx, td); err != nil {
		log.Printf("delegate: task maintenance observation deferred: %v", err)
	}
}
