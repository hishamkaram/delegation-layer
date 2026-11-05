package tasklifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const collectionSupervisorReconcileBackoff = 100 * time.Millisecond

func reconcileCollectionTimeout(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, options CollectionOptions, response *Observation) error {
	if ctx == nil {
		return context.Canceled
	}
	records, err := td.ReadStopRecords()
	if err != nil || !budgetStopNeedsObservation(records) || !options.ObserveSupervisor {
		return err
	}
	_, _, spec, metaHash, err := td.PreparedIdentity()
	if err != nil {
		return err
	}
	observation, reconcileErr := ReconcileStatus(ctx, root, td, req, meta, spec, metaHash, options.Supervisor, response)
	if response.MaintenanceError != nil && options.ReportMaintenanceDeferral != nil {
		options.ReportMaintenanceDeferral(response.MaintenanceError)
	}
	if reconcileErr != nil {
		return collectionSupervisorError(ctx, reconcileErr)
	}

	if err := UpdateStatusStops(ctx, td, response, observation); err != nil {
		// A bounded watch may expire while the durable stop annotation is being
		// written. The next collection pass can retry that annotation; the
		// current pass must remain a pending observation rather than an error.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return err
	}
	return nil
}

func reconcileCollectionTimeoutIfActive(ctx context.Context, deadline, nextAttempt time.Time, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, options CollectionOptions, response *Observation) (time.Time, error) {
	now := time.Now()
	if options.Watch <= 0 || !now.Before(deadline) || now.Before(nextAttempt) {
		return nextAttempt, nil
	}
	watchContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	err := reconcileCollectionTimeout(watchContext, root, td, req, meta, options, response)
	return time.Now().Add(collectionSupervisorReconcileBackoff), err
}

// A retained native owner is a concrete cleanup fault even when its underlying
// cause is also a timeout or a normally pending supervisor observation.
func collectionSupervisorError(ctx context.Context, err error) error {
	if admissionCleanupFault(err) {
		return err
	}
	if IsPureSupervisorUncertaintyError(err) {
		return reapSupervisorObservation(ctx, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}
