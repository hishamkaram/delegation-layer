package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// recoverInspectionLifetime is the sole inspection policy that turns an ended
// worker observation into lifetime termination authority. Only an operation
// with neither lifetime intent nor registration belongs to the historical path.
func recoverInspectionLifetime(ctx context.Context, operation *inspection.Operation) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if operation == nil {
		return task.ErrEvidenceFault
	}

	intent, err := optionalLifetimeIntent(operation.ReadLifetimeIntentContext(ctx))
	if err != nil {
		return err
	}
	registration, err := optionalLifetimeRegistration(operation.ReadLifetimeRegistrationContext(ctx))
	if err != nil {
		return err
	}
	if intent == nil && registration == nil {
		return nil
	}
	owner, err := operation.AcquireLifetimeOwner(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Release()) }()

	return reconcileInspectionLifetimeOwned(ctx, operation)
}

// reconcileInspectionLifetimeOwned applies the inspection recovery policy
// while its caller retains the lifetime-owner lease. It is shared by external
// recovery and the live guardian's deterministic scope-collision path.
func reconcileInspectionLifetimeOwned(ctx context.Context, operation *inspection.Operation) error {
	if ctx == nil {
		return context.Canceled
	}
	if operation == nil {
		return task.ErrEvidenceFault
	}
	intent, err := optionalLifetimeIntent(operation.ReadLifetimeIntentContext(ctx))
	if err != nil {
		return err
	}
	registration, err := optionalLifetimeRegistration(operation.ReadLifetimeRegistrationContext(ctx))
	if err != nil {
		return err
	}
	if intent == nil && registration == nil {
		return task.ErrEvidenceFault
	}
	var worker *task.LifetimeWorkerRecord
	var workerWait *task.LifetimeWorkerWaitRecord
	var launches []task.LifetimeLaunchRecord
	terminated := false
	if registration != nil {
		worker, err = optionalLifetimeWorker(operation.ReadLifetimeWorkerContext(ctx))
		if err != nil {
			return err
		}
		workerWait, err = optionalLifetimeWorkerWait(operation.ReadLifetimeWorkerWaitContext(ctx))
		if err != nil {
			return err
		}
		launches, err = operation.ReadLifetimeLaunchesContext(ctx)
		if err != nil {
			return err
		}
		terminated, err = operation.HasTerminatedLifetimeContext(ctx, "")
		if err != nil {
			return err
		}
	}
	return reconcileOwnedLifetime(ctx, intent, registration, worker, workerWait, launches, terminated, operation.RecordLifetimeTerminationContext)
}
