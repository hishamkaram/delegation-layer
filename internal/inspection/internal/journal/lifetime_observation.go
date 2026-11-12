package journal

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ObserveLifetimeContext reads one existing canonical operation while holding
// only nonblocking leases. The reader passed to observe expires with its
// transaction; the callback must not retain it or start external work.
func ObserveLifetimeContext(ctx context.Context, store *taskdir.Store, id string, observe func(taskdir.ControlLifetimeObservation) error) (resultErr error) {
	if store == nil || observe == nil {
		return task.ErrInvalidPermit
	}
	control, err := store.TryOpenInspectionContext(ctx, id)
	if err != nil {
		return err
	}
	defer func() { resultErr = control.CloseWithError(resultErr) }()
	owner, present, err := control.TryAcquireExistingLifetimeOwnerContext(ctx)
	if owner != nil {
		defer func() { resultErr = owner.ReleaseWithError(resultErr) }()
	}
	if err != nil {
		return err
	}
	return control.TryWithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		request, digest, deadline, readErr := readOperationRequestTx(ctx, tx, store, id)
		if readErr != nil {
			return readErr
		}
		lifetime, readErr := taskdir.NewControlLifetime(control, store.RootID, id, task.LifetimeOwnerInspection, digest, deadline)
		if readErr != nil {
			return readErr
		}
		records, readErr := lifetime.TransactionReader(tx)
		if readErr != nil {
			return readErr
		}
		preparationOnly, readErr := inspectionPreparationOnlyTx(tx, request, digest, deadline)
		if readErr != nil {
			return readErr
		}
		return observe(taskdir.ControlLifetimeObservation{Records: records, OwnerPresent: present, PreparationOnly: preparationOnly})
	})
}

func inspectionPreparationOnlyTx(tx *taskdir.ControlTransaction, request contract.RequestRecord, digest string, deadline time.Time) (bool, error) {
	if request.Binding.WorkerExitProof != contract.WorkerExitProofGuardianWaitV1 {
		return false, nil
	}
	names, err := tx.RecordNames()
	if err != nil {
		return false, err
	}
	for _, name := range names {
		switch name {
		case ".operation.lock", ".lifetime-owner.lock", requestRecordName:
		case submissionRecordName:
			var submission contract.SubmissionRecord
			if err = tx.Read(name, &submission); err != nil {
				return false, err
			}
			createdFloor, floorErr := contract.CanonicalTimestamp("inspection creation", request.CreatedAt)
			if floorErr != nil {
				return false, floorErr
			}
			if err = validateOperationClaim(submission, digest, createdFloor, deadline); err != nil {
				return false, err
			}
			if err = tx.Verify(name, submission); err != nil {
				return false, err
			}
		default:
			return false, nil
		}
	}
	return true, nil
}
