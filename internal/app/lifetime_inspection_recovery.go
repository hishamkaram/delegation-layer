package app

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
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
	if _, err := readAuxiliaryLifetimeProof(ctx, operation); err == nil {
		return nil
	} else if !errors.Is(err, workspace.ErrPending) {
		return err
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
		if _, fenceErr := operation.ReadLifetimeUnlaunchedContext(ctx); fenceErr == nil {
			return nil
		} else if !task.RecordAbsent(fenceErr) {
			return fenceErr
		}
		if operation.Request().Binding.WorkerExitProof == inspection.WorkerExitProofGuardianWaitV1 && operation.Expired(time.Now()) {
			_, fenceErr := operation.SealLifetimeUnlaunchedContext(ctx)
			return fenceErr
		}
		return nil
	}
	owner, err := operation.TryAcquireLifetimeOwner(ctx)
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
	if operation == nil {
		return task.ErrEvidenceFault
	}
	return reconcileControlLifetimeOwned(ctx, operation)
}

// controlLifetimeRecovery is shared by finite auxiliary operations. The
// operation family supplies storage; reconciliation owns all closure policy.
type controlLifetimeRecovery interface {
	lifetimeRecoveryRecords
}

func reconcileControlLifetimeOwned(ctx context.Context, operation controlLifetimeRecovery) error {
	return reconcileLifetimeRecordsOwned(ctx, operation)
}
