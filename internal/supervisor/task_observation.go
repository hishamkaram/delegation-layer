package supervisor

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ObserveTaskSupervisor keeps existing-task observation independent of fresh
// admission. Complete generations retain the existing scheduling and runner
// repair owners. A prepared generation can only use saved-binding recovery.
func ObserveTaskSupervisor(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, baseOptions pueue.Options, initialExecutable, runnerExecutable string, openStore StoreOpener) (TaskSupervisorObservation, error) {
	if store == nil || meta == nil || !pueue.IsPrivateConfig(store.Root, binding.ConfigPath) {
		return TaskSupervisorObservation{}, nil
	}
	if ctx == nil {
		return TaskSupervisorObservation{}, context.Canceled
	}
	ctx, cancel := context.WithTimeout(ctx, pueue.RunnerCommandUpgradeTimeout)
	defer cancel()
	options := OptionsWithEnvironment(baseOptions, meta.Environment)
	client, err := ProbePrivateAdmissionSupervisor(ctx, store, "", "", options)
	if err != nil {
		return TaskSupervisorObservation{}, err
	}
	if client != nil {
		client, err = prepareTaskSupervisorRunner(ctx, store, client, binding, runnerExecutable)
	} else {
		observed, complete, completeErr := recoverTaskObservationSupervisor(ctx, store.Root, binding, options, initialExecutable, openStore)
		if completeErr != nil {
			return TaskSupervisorObservation{}, completeErr
		}
		if !complete {
			return TaskSupervisorObservation{Client: observed, MaintenanceDeferred: true}, nil
		}
		if closeErr := observed.Close(); closeErr != nil {
			return TaskSupervisorObservation{}, closeErr
		}
		client, err = PrepareTaskSupervisor(ctx, store, binding, meta, baseOptions, initialExecutable, runnerExecutable)
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
	observed, observeErr := NewClient(ctx, store.Root, binding, options, true, initialExecutable, openStore)
	if observeErr != nil {
		return TaskSupervisorObservation{}, errors.Join(err, observeErr)
	}
	return TaskSupervisorObservation{Client: observed, MaintenanceDeferred: true, MaintenanceError: err}, nil
}

func recoverTaskObservationSupervisor(ctx context.Context, root string, saved task.SupervisorRef, options pueue.Options, initialExecutable string, openStore StoreOpener) (*pueue.Client, bool, error) {
	client, err := NewClient(ctx, root, saved, options, true, initialExecutable, openStore)
	if err != nil {
		return nil, false, err
	}
	complete, err := taskSupervisorMaintenanceComplete(ctx, root, client.RuntimeBinding())
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
