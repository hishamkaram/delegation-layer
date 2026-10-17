package admission

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/inspection"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// recoverInspectionLifetime is the sole inspection policy that turns an ended
// worker observation into lifetime termination authority. Only an operation
// with neither lifetime intent nor registration belongs to the historical path.
func RecoverInspectionLifetime(ctx context.Context, operation *inspection.Operation) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if operation == nil {
		return task.ErrEvidenceFault
	}
	if _, err := guardian.ReadAuxiliaryClosureProof(ctx, operation); err == nil {
		return nil
	} else if !errors.Is(err, guardian.ErrProofPending) {
		return err
	}

	intent, err := guardian.OptionalIntent(operation.ReadLifetimeIntentContext(ctx))
	if err != nil {
		return err
	}
	registration, err := guardian.OptionalRegistration(operation.ReadLifetimeRegistrationContext(ctx))
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

	return ReconcileInspectionLifetimeOwned(ctx, operation)
}

// reconcileInspectionLifetimeOwned applies the inspection recovery policy
// while its caller retains the lifetime-owner lease. It is shared by external
// recovery and the live guardian's deterministic scope-collision path.
func ReconcileInspectionLifetimeOwned(ctx context.Context, operation *inspection.Operation) error {
	if operation == nil {
		return task.ErrEvidenceFault
	}
	return guardian.ReconcileRecordsOwned(ctx, operation)
}
