package supervisor

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// TaskSupervisorObservationOptions groups immutable runtime dependencies and
// the caller's actual borrowed SH resource. Pure ready observations borrow no
// new native resource; repair owns acquisition before generation/bootstrap work.
type TaskSupervisorObservationOptions struct {
	Control          pueue.Options
	Lookup           ExecutableLookup
	RunnerExecutable string
	OpenStore        StoreOpener
	RootAdmission    *taskdir.RootAdmissionLease
	Routing          OwnerRouting
	Source           infrastructure.SavedBindingSource
}

// ObserveTaskSupervisor keeps existing-task observation independent of fresh
// admission. Complete generations retain the existing scheduling and runner
// repair owners. A prepared generation can only use saved-binding recovery.
func ObserveTaskSupervisor(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, baseOptions pueue.Options, lookup ExecutableLookup, runnerExecutable string, openStore StoreOpener) (TaskSupervisorObservation, error) {
	return ObserveTaskSupervisorWithAdmissionContext(ctx, store, binding, meta, TaskSupervisorObservationOptions{Control: baseOptions, Lookup: lookup, RunnerExecutable: runnerExecutable, OpenStore: openStore})
}

// ObserveTaskSupervisorWithAdmissionContext keeps the warm ready prefix
// observational. Any nonnil Client is caller-owned, including beside an error.
// A failed owned RootSH release remains reachable through its cleanup error;
// the borrowed Store must stay open until that resource closes.
func ObserveTaskSupervisorWithAdmissionContext(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, options TaskSupervisorObservationOptions) (result TaskSupervisorObservation, resultErr error) {
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
	lease, err := AcquireOrBorrowRootAdmissionContext(ctx, store, options.RootAdmission, pueue.PrivateControlTimeout(control))
	if lease != nil && options.RootAdmission == nil {
		defer func() { resultErr = ReleaseOwnedRootAdmission(lease, resultErr) }()
	}
	if err != nil {
		return TaskSupervisorObservation{}, err
	}
	if control.ControlOwnerExecutable != "" {
		ctx = pueue.WithControlLease(ctx, lease)
	}
	// The readiness observation ended before acquisition. Reprobe under the
	// actual outer root hold, then use the existing canonical repair owner.
	return observeTaskSupervisorRepair(ctx, store, binding, meta, options)
}

func probeReadyTaskSupervisor(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, control pueue.Options, runnerExecutable string) (*pueue.Client, error) {
	client, err := ProbePrivateAdmissionSupervisor(ctx, store, "", "", control)
	if err != nil || client == nil {
		return client, err
	}
	target, err := PlanQueuedRunnerRepairTarget(store, runnerExecutable)
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
	client, err := ProbePrivateAdmissionSupervisor(ctx, store, "", "", options)
	if err != nil {
		return TaskSupervisorObservation{}, err
	}
	if client != nil {
		client, err = prepareTaskSupervisorRunner(ctx, store, client, binding, configuration.RunnerExecutable)
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
		return TaskSupervisorObservation{}, ClosePrivateAdmissionTransition(client, err)
	}
	// A readiness result never authorizes the later repair. If migration won
	// its EX lease first, recover the saved task afresh instead of accepting
	// the old client or treating the refused repair as a successful mutation.
	observed, observeErr := recoverObservedTaskClient(ctx, store, binding, meta, configuration)
	if observeErr != nil {
		return TaskSupervisorObservation{}, errors.Join(err, ClosePrivateAdmissionTransition(observed, observeErr))
	}
	return TaskSupervisorObservation{Client: observed, MaintenanceDeferred: true, MaintenanceError: err}, nil
}

func recoverTaskObservationSupervisor(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef, meta *task.MetaRecord, configuration TaskSupervisorObservationOptions) (*pueue.Client, bool, error) {
	client, err := recoverObservedTaskClient(ctx, store, saved, meta, configuration)
	if err != nil {
		return nil, false, ClosePrivateAdmissionTransition(client, err)
	}
	complete, err := taskSupervisorMaintenanceComplete(ctx, store.Root, client.RuntimeBinding())
	if err != nil {
		return nil, false, ClosePrivateAdmissionTransition(client, err)
	}
	return client, complete, nil
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
