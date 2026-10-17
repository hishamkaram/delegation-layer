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
	if reconcileErr != nil {
		if IsPureSupervisorUncertaintyError(reconcileErr) {
			return reapSupervisorObservation(ctx, reconcileErr)
		}
		if errors.Is(reconcileErr, context.Canceled) || errors.Is(reconcileErr, context.DeadlineExceeded) {
			return nil
		}
		return reconcileErr
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

func reconcileCollectionTimeoutIfActive(deadline, nextAttempt time.Time, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, options CollectionOptions, response *Observation) (time.Time, error) {
	now := time.Now()
	if options.Watch <= 0 || !now.Before(deadline) || now.Before(nextAttempt) {
		return nextAttempt, nil
	}
	watchContext, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	err := reconcileCollectionTimeout(watchContext, root, td, req, meta, options, response)
	return time.Now().Add(collectionSupervisorReconcileBackoff), err
}
