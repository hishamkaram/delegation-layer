package supervisor

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// TaskSupervisorObservationOptions groups immutable runtime dependencies and
// the caller's actual Store. Generation/bootstrap owners serialize repair;
// pure ready observations acquire no new outer resource.
type TaskSupervisorObservationOptions struct {
	Control          pueue.Options
	Lookup           installation.ExecutableLookup
	RunnerExecutable string
	OpenStore        StoreOpener
	Routing          OwnerRouting
	Source           infrastructure.SavedBindingSource
}

// ObserveTaskSupervisorWithOptions keeps the warm ready prefix
// observational. Any nonnil Client is caller-owned, including beside an error.
func ObserveTaskSupervisorWithOptions(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, options TaskSupervisorObservationOptions) (result TaskSupervisorObservation, resultErr error) {
	if store == nil || meta == nil || !pueue.IsPrivateConfig(store.Root, binding.ConfigPath) {
		return TaskSupervisorObservation{}, nil
	}
	if ctx == nil {
		return TaskSupervisorObservation{}, context.Canceled
	}
	ctx, cancel := context.WithTimeout(ctx, pueue.RunnerCommandUpgradeTimeout)
	defer cancel()
	if _, err := store.PinRootContext(ctx); err != nil {
		return TaskSupervisorObservation{}, err
	}
	control := OptionsWithEnvironment(options.Control, meta.Environment)
	ready, err := probeReadyTaskSupervisor(ctx, store, binding, control, options.RunnerExecutable)
	if err != nil {
		return TaskSupervisorObservation{}, err
	}
	if ready != nil {
		if _, pinErr := store.PinRootContext(ctx); pinErr != nil {
			return TaskSupervisorObservation{}, ClosePrivateAdmissionTransition(ready, pinErr)
		}
		return TaskSupervisorObservation{Client: ready}, nil
	}
	return observeTaskSupervisorRepair(ctx, store, binding, meta, options)
}

func probeReadyTaskSupervisor(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, control pueue.Options, runnerExecutable string) (*pueue.Client, error) {
	client, err := probeWarmTaskSupervisor(ctx, store, control)
	if err != nil || client == nil {
		return client, err
	}
	target, err := PlanQueuedRunnerRepairTarget(runnerExecutable)
	if err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	receipt, name, err := newRunnerRepairReceipt(store, client, target)
	if err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	current, err := runnerRepairReceiptCurrent(ctx, store, name, receipt)
	if err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	if !current {
		return nil, client.Close()
	}
	if err = client.ScopePrivateTask(ctx, store.Root, binding); err != nil {
		return nil, ClosePrivateAdmissionTransition(client, err)
	}
	return client, nil
}

// A missing or historical copied pair defers the exact warm probe. Saved-task
// observation validates installed transport selection under its shared lease.
func probeWarmTaskSupervisor(ctx context.Context, store *taskdir.Store, options pueue.Options) (*pueue.Client, error) {
	published, found, err := pueue.ReadPrivateSupervisorBinding(store.Root)
	if err != nil {
		return nil, err
	}
	if found {
		if pueue.IsLegacyStateSupervisorClient(store.Root, published) {
			return nil, nil
		}
		missing, missingErr := pueue.PrivateSupervisorPairMissing(published)
		if missingErr != nil || missing {
			return nil, missingErr
		}
	}
	return ProbePrivateAdmissionSupervisor(ctx, store, "", "", options)
}

func observeTaskSupervisorRepair(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, configuration TaskSupervisorObservationOptions) (TaskSupervisorObservation, error) {
	if store == nil || meta == nil || !pueue.IsPrivateConfig(store.Root, binding.ConfigPath) {
		return TaskSupervisorObservation{}, nil
	}
	if ctx == nil {
		return TaskSupervisorObservation{}, context.Canceled
	}
	ctx, cancel := context.WithTimeout(ctx, pueue.RunnerCommandUpgradeTimeout)
	defer cancel()
	options := OptionsWithEnvironment(configuration.Control, meta.Environment)
	client, err := probeWarmTaskSupervisor(ctx, store, options)
	if err != nil {
		return TaskSupervisorObservation{}, err
	}
	if client != nil {
		client, err = prepareWarmTaskSupervisorRunner(ctx, store, client, binding, configuration.RunnerExecutable)
	} else {
		observed, complete, completeErr := recoverTaskObservationSupervisor(ctx, store, binding, meta, configuration)
		if completeErr != nil {
			return TaskSupervisorObservation{}, completeErr
		}
		if !complete {
			return TaskSupervisorObservation{Client: observed, MaintenanceDeferred: true}, nil
		}
		if closeErr := observed.Close(); closeErr != nil {
			return TaskSupervisorObservation{}, closeErr
		}
		client, err = PrepareTaskSupervisorWithRouting(ctx, store, binding, meta, configuration)
	}
	if client != nil && err == nil {
		return TaskSupervisorObservation{Client: client}, nil
	}
	if err != nil && !TaskMaintenanceCompletionRefused(err) {
		return TaskSupervisorObservation{}, err
	}
	// A readiness result never authorizes the later repair. If migration won
	// its EX lease first, recover the saved task afresh instead of accepting
	// the old client or treating the refused repair as a successful mutation.
	observed, observeErr := recoverObservedTaskReadClient(ctx, store, binding, meta, configuration)
	if observeErr != nil {
		return TaskSupervisorObservation{}, errors.Join(err, ClosePrivateAdmissionTransition(observed, observeErr))
	}
	return TaskSupervisorObservation{Client: observed, MaintenanceDeferred: true, MaintenanceError: err}, nil
}

func recoverTaskObservationSupervisor(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef, meta *task.MetaRecord, configuration TaskSupervisorObservationOptions) (*pueue.Client, bool, error) {
	client, err := recoverObservedTaskReadClient(ctx, store, saved, meta, configuration)
	if err != nil {
		return nil, false, ClosePrivateAdmissionTransition(client, err)
	}
	complete, err := taskSupervisorMaintenanceComplete(ctx, store.Root, client.RuntimeBinding())
	if err != nil {
		return nil, false, ClosePrivateAdmissionTransition(client, err)
	}
	return client, complete, nil
}

func recoverObservedTaskReadClient(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, configuration TaskSupervisorObservationOptions) (*pueue.Client, error) {
	runtime, source, err := taskSavedRecoveryConfiguration(meta, configuration)
	if err != nil {
		return nil, err
	}
	return runtime.RecoverSavedTaskObservation(ctx, store, binding, source)
}

// This is eligibility only. The existing transition and mutation owners
// recheck the canonical completion predicate under their own mutation leases.
func taskSupervisorMaintenanceComplete(ctx context.Context, root string, runtime task.SupervisorRef) (complete bool, resultErr error) {
	if !pueue.IsPrivateConfig(root, runtime.ConfigPath) {
		// A validated recovery alias may observe its retained endpoint, but it
		// never grants maintenance authority over the published generation.
		return false, nil
	}
	resultErr = pueue.WithSupervisorLeaseContext(ctx, root, func(lockedContext context.Context) error {
		published, found, err := pueue.ReadPrivateSupervisorBinding(root)
		if err != nil {
			return err
		}
		// Optional installation maintenance needs the published pair. A
		// compatible observation client must not reopen that unavailable pair.
		if !found || published != runtime {
			return lockedContext.Err()
		}
		complete, err = pueue.PrivateSupervisorMigrationComplete(root, published)
		return errors.Join(err, lockedContext.Err())
	})
	return complete, resultErr
}
