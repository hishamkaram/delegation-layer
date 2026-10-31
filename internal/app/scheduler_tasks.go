package app

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	schedulerruntime "github.com/hishamkaram/delegation-layer/internal/scheduler/runtime"
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
	arguments.Brief, arguments.BriefBytes = "", definition.Brief
	options := dispatchOptions(tasks.dependencies, false)
	options.OpenStore = tasks.openStore
	dispatched := tasklifecycle.Dispatch(dispatchRequest(arguments, tasks.dependencies), options)
	// WaitForPendingClose makes the ordinary lifecycle's deferred close join its
	// existing owner. Observing/fencing now cannot race this invocation's launch.
	state, observeErr := tasks.Observe(request.TaskID)
	if dispatched.Err() != nil && state.Observation.FaultCode == "" {
		state.Observation.FaultCode = "admission_failed"
	}
	return state, errors.Join(dispatched.Err(), observeErr)
}

func (tasks schedulerTasks) Observe(id string) (state schedulerruntime.TaskState, resultErr error) {
	defer func() {
		if resultErr != nil {
			state.Release = nil
			// An incomplete read does not replace the last validated observation.
			state.Observation = contract.Observation{}
		}
	}()
	state.Observation = contract.Observation{ObservedAt: time.Now().UTC()}
	store, err := tasks.openStore(tasks.root, false)
	if err != nil {
		return state, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	exists, err := store.TaskEntryExists(id)
	if err != nil {
		return state, err
	}
	if !exists {
		state.Observation.Admission = task.AdmissionNotAdmitted
		state.Release = &contract.Release{ReleasedAt: time.Now().UTC(), Kind: contract.ReleaseNotCreated, Ref: "task_absent"}
		return state, nil
	}
	td, err := store.OpenTask(id)
	if err != nil {
		// Missing locks in an existing directory are partial evidence.
		return state, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	options := observationSupervisorOptions(tasks.dependencies.supervisorControlScope(), tasks.dependencies.supervisorLookup())
	observed, err := tasklifecycle.Status(store, td, options, tasks.dependencies.RunnerExecutable, tasks.dependencies.Catalog)
	state.Observation, resultErr = schedulerObservation(td, observed)
	if err != nil || resultErr != nil {
		state.Observation.FaultCode = "observation_failed"
		return state, errors.Join(err, resultErr)
	}
	proof, err := guardian.ReadClosureProof(context.Background(), td)
	if err == nil && proof.Closed() {
		ref := proof.TerminationSHA256()
		if ref == "" {
			ref = proof.WorkerWaitSHA256()
		}
		state.Release = &contract.Release{ReleasedAt: time.Now().UTC(), Kind: contract.ReleaseClosed, Ref: ref}
		return state, nil
	}
	if !errors.Is(err, guardian.ErrProofPending) {
		return state, err
	}
	return tasks.observeNeverStarted(td, observed, state)
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

func (tasks schedulerTasks) observeNeverStarted(td *taskdir.TaskDir, observed tasklifecycle.StatusResult, state schedulerruntime.TaskState) (schedulerruntime.TaskState, error) {
	if observed.Pending != "" || !observed.AdmissionObserved || observed.Admission != task.AdmissionNotAdmitted {
		return state, nil
	}
	proven, ref, err := tasklifecycle.FenceScheduledTaskContext(context.Background(), td)
	if err != nil {
		return state, err
	}
	if proven {
		state.Release = &contract.Release{ReleasedAt: time.Now().UTC(), Kind: contract.ReleaseClosed, Ref: ref}
	}
	return state, nil
}
