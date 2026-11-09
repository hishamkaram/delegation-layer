package app

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	schedulerruntime "github.com/hishamkaram/delegation-layer/internal/scheduler/runtime"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// schedulerTasks adapts the existing lifecycle to the loop's narrow port. Each
// invocation has its own store and control scope; no dispatch state is shared.
type schedulerTasks struct {
	root         string
	rootID       string
	arguments    Arguments
	dependencies Dependencies
	routing      supervisor.OwnerRouting
}

func (tasks schedulerTasks) openStore(root string, _ bool) (*taskdir.Store, error) {
	store, err := openStore(root, tasks.dependencies.storeDependencies(), false)
	if err != nil {
		return nil, err
	}
	if store.RootID != tasks.rootID {
		return nil, errors.Join(schedulerruntime.ErrRootIdentity, task.ErrIdentityMismatch, store.Close())
	}
	return store, nil
}

func (tasks schedulerTasks) Dispatch(request schedulerruntime.DispatchRequest) (schedulerruntime.TaskState, error) {
	definition := request.Definition.Clone()
	arguments := tasks.arguments
	arguments.Root, arguments.TaskID = tasks.root, request.TaskID
	arguments.Provider, arguments.Auto = definition.Request.Provider, definition.Request.Auto
	arguments.Config, arguments.Cwd = definition.Request.Config, definition.Request.CanonicalCwd
	arguments.teamTarget = definition.Request.Team
	arguments.Brief, arguments.BriefBytes = "", definition.Brief
	options := dispatchOptions(tasks.dependencies, false)
	options.OpenStore = tasks.openStore
	options.Creation.Admission.Supervisor.Routing = tasks.routing
	dispatched := tasklifecycle.Dispatch(dispatchRequest(arguments, tasks.dependencies), options)
	projected := dispatchCommandResult(dispatched)
	attempt := schedulerAdmissionFailure(projected.err, projected.response.TaskRecord)
	if cleanupErr := schedulerTaskCleanupError(projected.err); errors.Is(cleanupErr, schedulerruntime.ErrTaskCleanupUnresolved) {
		return schedulerruntime.TaskState{Attempt: attempt}, cleanupErr
	}
	// WaitForPendingClose makes the ordinary lifecycle's deferred close join its
	// existing owner. Observing/fencing now cannot race this invocation's launch.
	state, observeErr := tasks.Observe(request.TaskID)
	if projected.err != nil && state.Observation.FaultCode == "" {
		state.Observation.FaultCode = "admission_failed"
	}
	state.Attempt = attempt
	return state, errors.Join(projected.err, observeErr)
}

func (tasks schedulerTasks) Observe(id string) (state schedulerruntime.TaskState, resultErr error) {
	store, err := tasks.openStore(tasks.root, false)
	if err != nil {
		return state, err
	}
	defer func() { closeSchedulerObservation(&state, &resultErr, store) }()
	exists, err := store.TaskEntryExists(id)
	if err != nil {
		return state, err
	}
	if !exists {
		state.Observation = contract.Observation{ObservedAt: time.Now().UTC()}
		state.Observation.Admission = task.AdmissionNotAdmitted
		state.Release = &contract.Release{ReleasedAt: time.Now().UTC(), Kind: contract.ReleaseNotCreated, Ref: "task_absent"}
		return state, nil
	}
	td, err := store.OpenTask(id)
	if err != nil {
		// Missing locks in an existing directory are partial evidence.
		return state, err
	}
	defer func() { clearSchedulerStateOnCloseFault(&state, &resultErr, td.Close()) }()
	options := observationSupervisorOptions(tasks.dependencies.supervisorControlScope(), tasks.dependencies.supervisorLookup(), tasks.routing)
	observed, err := tasklifecycle.Status(store, td, options, tasks.dependencies.RunnerExecutable, tasks.dependencies.Catalog)
	return tasks.observeStatus(td, observed, err)
}

func closeSchedulerObservation(state *schedulerruntime.TaskState, resultErr *error, store *taskdir.Store) {
	if !cleanupRetainsStore(*resultErr, store) {
		clearSchedulerStateOnCloseFault(state, resultErr, store.Close())
	}
	*resultErr = schedulerTaskCleanupError(*resultErr)
}

// The scheduler runtime owns the fatal classification. This boundary preserves
// the actual application cleanup projections that keep their resources alive.
func schedulerTaskCleanupError(cause error) error {
	unresolved := tasklifecycle.CleanupUnresolved(cause) || task.AnyErrorNode(cause, func(node error) bool {
		var cleanup *taskAdmissionCleanupError
		return errors.As(node, &cleanup) && cleanup != nil && cleanup.store != nil
	})
	if !unresolved || errors.Is(cause, schedulerruntime.ErrTaskCleanupUnresolved) {
		return cause
	}
	return errors.Join(schedulerruntime.ErrTaskCleanupUnresolved, cause)
}

// Local read/close faults discard this pass; task diagnostics accompanying a
// fully validated observation and release do not erase those independent facts.
func clearSchedulerStateOnCloseFault(state *schedulerruntime.TaskState, resultErr *error, closeErr error) {
	if closeErr != nil {
		*state = schedulerruntime.TaskState{}
		*resultErr = errors.Join(*resultErr, closeErr)
	}
}

func (tasks schedulerTasks) observeStatus(td *taskdir.TaskDir, observed tasklifecycle.StatusResult, statusErr error) (schedulerruntime.TaskState, error) {
	observation, err := schedulerObservation(td, observed)
	if err != nil {
		return schedulerruntime.TaskState{}, errors.Join(statusErr, err)
	}
	state := schedulerruntime.TaskState{Observation: observation}
	if statusErr != nil {
		if !observed.ControlObservation || !tasklifecycle.IsPureSupervisorUncertaintyError(statusErr) {
			return schedulerruntime.TaskState{}, statusErr
		}
		// A missing queue row is not closure. Only the existing permanent
		// refusal plus complete never-launched proof can resolve this case.
		state, err = tasks.observeNeverStarted(td, observed, state, false)
		if err != nil || state.Release == nil {
			return schedulerruntime.TaskState{}, errors.Join(statusErr, err)
		}
		state.Observation.FaultCode = "supervisor_unknown"
		return state, statusErr
	}
	proof, err := guardian.ReadClosureProof(context.Background(), td)
	if err == nil && proof.Closed() {
		ref := proof.TerminationSHA256()
		if ref == "" {
			ref = proof.WorkerWaitSHA256()
		}
		state.Release = &contract.Release{ReleasedAt: time.Now().UTC(), Kind: contract.ReleaseClosed, Ref: ref}
		return tasks.collectAtClosure(td, observed, state)
	}
	if !errors.Is(err, guardian.ErrProofPending) {
		return schedulerruntime.TaskState{}, err
	}
	return tasks.observeNeverStarted(td, observed, state, true)
}

// Collect owns recovery from the existing seal. Native closure has already
// been validated; a diagnostic from Collect cannot invalidate that proof when
// the subsequent local evidence read succeeds.
func (tasks schedulerTasks) collectAtClosure(td *taskdir.TaskDir, observed tasklifecycle.StatusResult, state schedulerruntime.TaskState) (schedulerruntime.TaskState, error) {
	if state.Observation.Publication != task.PublicationPending {
		return state, nil
	}
	options := tasks.dependencies.storeDependencies().collectionOptions(0)
	options.ObserveSupervisor = false
	collected, collectErr := tasklifecycle.Collect(tasks.root, td, options)
	inspection, err := td.Inspect()
	if err != nil {
		return schedulerruntime.TaskState{}, errors.Join(collectErr, err)
	}
	observed.Inspection = inspection
	state.Observation, err = schedulerObservation(td, observed)
	if err != nil {
		return schedulerruntime.TaskState{}, errors.Join(collectErr, err)
	}
	switch {
	case collectErr != nil && !tasklifecycle.IsPureCollectionPendingError(collectErr):
		state.Observation.FaultCode = "collection_failed"
	case collectErr != nil || collected.Waiting || collected.Pending != "":
		state.Observation.FaultCode = "collection_pending"
	}
	return state, collectErr
}

func schedulerObservation(td *taskdir.TaskDir, observed tasklifecycle.StatusResult) (contract.Observation, error) {
	observation := contract.Observation{
		ObservedAt: time.Now().UTC(), Admission: observed.Admission, Liveness: observed.Liveness,
	}
	if observed.Inspection == nil {
		return observation, nil
	}
	observation.Publication = observed.Inspection.Publication
	if observed.Inspection.Outcome == nil {
		return observation, nil
	}
	ref, err := td.OutcomeReference()
	if err != nil {
		observation.Publication = task.PublicationUnknown
		return observation, err
	}
	observation.OutcomeSHA256 = ref
	return observation, nil
}

func (tasks schedulerTasks) observeNeverStarted(td *taskdir.TaskDir, observed tasklifecycle.StatusResult, state schedulerruntime.TaskState, allowFence bool) (schedulerruntime.TaskState, error) {
	if observed.Pending != "" {
		return state, nil
	}
	var proven bool
	var ref string
	var err error
	if allowFence && observed.AdmissionObserved && observed.Admission == task.AdmissionNotAdmitted {
		proven, ref, err = tasklifecycle.FenceScheduledTaskContext(context.Background(), td)
	} else {
		proven, ref, err = tasklifecycle.ProveScheduledTaskUnlaunchedContext(context.Background(), td)
	}
	if err != nil {
		return schedulerruntime.TaskState{}, err
	}
	if proven {
		state.Release = &contract.Release{ReleasedAt: time.Now().UTC(), Kind: contract.ReleaseClosed, Ref: ref}
	}
	return state, nil
}

func schedulerAdmissionFailure(err error, record string) *contract.AttemptFailure {
	if err == nil {
		return nil
	}
	failure := dispatchFailure(err, record)
	next := failure.NextAction
	switch next {
	case "correct_request":
		next = "recreate_schedule"
	case "check_status":
		next = "inspect_schedule_execution"
	case "retry_later", "retry_dispatch", "watch_workspace":
		next = "wait_next_occurrence"
	}
	return &contract.AttemptFailure{Code: failure.Code, Stage: failure.Stage, NextAction: next, Message: dispatchFailureMessage(failure.Code)}
}
