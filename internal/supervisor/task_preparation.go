package supervisor

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func PrepareTaskSupervisorWithRouting(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, configuration TaskSupervisorObservationOptions) (*pueue.Client, error) {
	if store == nil || meta == nil || !pueue.IsPrivateConfig(store.Root, binding.ConfigPath) {
		return nil, nil
	}
	if ctx == nil {
		return nil, context.Canceled
	}
	ctx, cancel := context.WithTimeout(ctx, pueue.RunnerCommandUpgradeTimeout)
	defer cancel()
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		return nil, err
	}
	control := OptionsWithEnvironment(configuration.Control, meta.Environment)
	client, err := probeWarmTaskSupervisor(ctx, store, control)
	if err != nil {
		return nil, err
	}
	if client != nil {
		if err = checkInfrastructureTarget(ctx, store, pin); err == nil {
			err = client.ScopePrivateTask(ctx, store.Root, binding)
		}
	} else {
		client, err = recoverObservedTaskClient(ctx, store, binding, meta, configuration)
	}
	if err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	complete, err := taskSupervisorMaintenanceComplete(ctx, store.Root, client.RuntimeBinding())
	if err != nil || !complete {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	if err = EnsureAdmissionSchedulingContext(ctx, store.Root, client); err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	return repairTaskSupervisorRunner(ctx, store, client, configuration.RunnerExecutable)
}

func recoverObservedTaskClient(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, configuration TaskSupervisorObservationOptions) (*pueue.Client, error) {
	runtime, source, err := taskSavedRecoveryConfiguration(meta, configuration)
	if err != nil {
		return nil, err
	}
	return runtime.RecoverSaved(ctx, store, binding, source)
}

func taskSavedRecoveryConfiguration(meta *task.MetaRecord, configuration TaskSupervisorObservationOptions) (ControlRuntime, infrastructure.SavedBindingSource, error) {
	source := configuration.Source
	if source == (infrastructure.SavedBindingSource{}) {
		var err error
		source, err = NewSavedBindingSource(meta.RootID, infrastructure.SavedBindingTaskMetadata, meta.TaskID, "", meta.SupervisorConfig)
		if err != nil {
			return ControlRuntime{}, infrastructure.SavedBindingSource{}, err
		}
	}
	options := OptionsWithEnvironment(configuration.Control, meta.Environment)
	return ControlRuntime{Options: options, Lookup: configuration.Lookup, Routing: configuration.Routing}, source, nil
}

func repairTaskSupervisorRunner(ctx context.Context, store *taskdir.Store, client *pueue.Client, runnerExecutable string) (*pueue.Client, error) {
	target, err := PlanQueuedRunnerRepairTarget(runnerExecutable)
	if err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	if err = EnsurePrivateQueuedRunnerRepairContext(ctx, store, client, target); err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	return client, nil
}

// Warm admission clients still need task scoping; saved recovery already scopes.
func prepareWarmTaskSupervisorRunner(ctx context.Context, store *taskdir.Store, client *pueue.Client, binding task.SupervisorRef, runnerExecutable string) (*pueue.Client, error) {
	prepared, err := repairTaskSupervisorRunner(ctx, store, client, runnerExecutable)
	if err != nil {
		return nil, err
	}
	if err = prepared.ScopePrivateTask(ctx, store.Root, binding); err != nil {
		return nil, ClosePrivateAdmissionTransition(prepared, err)
	}
	return prepared, nil
}
