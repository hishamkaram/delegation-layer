package supervisor

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func PrepareTaskSupervisor(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, baseOptions pueue.Options, lookup ExecutableLookup, runnerExecutable string) (*pueue.Client, error) {
	return PrepareTaskSupervisorWithRouting(ctx, store, binding, meta, TaskSupervisorObservationOptions{Control: baseOptions, Lookup: lookup, RunnerExecutable: runnerExecutable})
}

func PrepareTaskSupervisorWithRouting(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, configuration TaskSupervisorObservationOptions) (*pueue.Client, error) {
	if store == nil || meta == nil || !pueue.IsPrivateConfig(store.Root, binding.ConfigPath) {
		return nil, nil
	}
	if ctx == nil {
		return nil, context.Canceled
	}
	ctx, cancel := context.WithTimeout(ctx, pueue.RunnerCommandUpgradeTimeout)
	defer cancel()
	client, err := recoverObservedTaskClient(ctx, store, binding, meta, configuration)
	if err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	complete, err := taskSupervisorMaintenanceComplete(ctx, store.Root, client.RuntimeBinding())
	if err != nil || !complete {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	return prepareTaskSupervisorRunner(ctx, store, client, binding, configuration.RunnerExecutable)
}

func recoverObservedTaskClient(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, configuration TaskSupervisorObservationOptions) (*pueue.Client, error) {
	source := configuration.Source
	if source == (infrastructure.SavedBindingSource{}) {
		var err error
		source, err = NewSavedBindingSource(meta.RootID, infrastructure.SavedBindingTaskMetadata, meta.TaskID, "", meta.SupervisorConfig)
		if err != nil {
			return nil, err
		}
	}
	options := OptionsWithEnvironment(configuration.Control, meta.Environment)
	return (ControlRuntime{Options: options, Lookup: configuration.Lookup, Routing: configuration.Routing}).RecoverSaved(ctx, store, binding, source)
}

func prepareTaskSupervisorRunner(ctx context.Context, store *taskdir.Store, client *pueue.Client, binding task.SupervisorRef, runnerExecutable string) (*pueue.Client, error) {
	target, err := PlanQueuedRunnerRepairTarget(store, runnerExecutable)
	if err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	if err = EnsurePrivateQueuedRunnerRepairContext(ctx, store, client, target); err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	if err = client.ScopePrivateTask(ctx, store.Root, binding); err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	return client, nil
}
