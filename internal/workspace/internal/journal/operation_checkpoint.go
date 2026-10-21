package journal

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

// A checkpoint creation journal owns its immutable candidate and qualified
// receipt beside the request. The worker writes the candidate inside its live
// lifetime; the coordinator writes the receipt after canonical closure.
const (
	checkpointCandidateName = "checkpoint-candidate.json"
	checkpointReceiptName   = "checkpoint-receipt.json"
)

// RecordCheckpointCandidate publishes the exact candidate before its owned ref
// is created. Only a live lifetime may create it; after closure an identical
// winner replays and any other candidate is refused.
func (o *Operation) RecordCheckpointCandidate(ctx context.Context, candidate contract.CheckpointCandidate) error {
	if err := contract.ValidateCheckpointCandidate(candidate, o.request, o.digest); err != nil {
		return err
	}
	return o.WithLifetimeLaunchPermissionOrReplayContext(ctx, func(tx *taskdir.ControlTransaction) error {
		if _, err := o.readCheckpointCandidateTx(tx); err != nil {
			return err
		}
		return tx.Verify(checkpointCandidateName, candidate)
	}, func(tx *taskdir.ControlTransaction) error {
		if _, err := tx.Put(checkpointCandidateName, candidate); err != nil {
			return err
		}
		return tx.Verify(checkpointCandidateName, candidate)
	})
}

// CheckpointCandidate reads the exact candidate bound to this request. An
// absent candidate is reported as absent, never fabricated.
func (o *Operation) CheckpointCandidate(ctx context.Context) (candidate contract.CheckpointCandidate, resultErr error) {
	resultErr = o.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		candidate, err = o.readCheckpointCandidateTx(tx)
		return err
	})
	return candidate, resultErr
}

// RecordCheckpointReceipt persists the coordinator's qualified creation
// disposition after canonical closure. It needs no launch permission and
// never decides the closure kind: only the coordinator's native proof may
// qualify a checkpointed receipt. A candidate echo must equal the stored
// candidate exactly.
func (o *Operation) RecordCheckpointReceipt(ctx context.Context, receipt contract.CheckpointReceipt) error {
	if err := contract.ValidateCheckpointReceipt(receipt, o.request, o.digest); err != nil {
		return err
	}
	return o.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := o.requireReceiptCandidateTx(tx, receipt, task.ErrIdentityMismatch); err != nil {
			return err
		}
		if _, err := tx.Put(checkpointReceiptName, receipt); err != nil {
			return err
		}
		return tx.Verify(checkpointReceiptName, receipt)
	})
}

// CheckpointReceipt reads the qualified creation disposition.
func (o *Operation) CheckpointReceipt(ctx context.Context) (receipt contract.CheckpointReceipt, resultErr error) {
	resultErr = o.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := o.requireCheckpointCreation(); err != nil {
			return err
		}
		if err := tx.Read(checkpointReceiptName, &receipt); err != nil {
			return classifyResourceRecordReadError(err)
		}
		if err := contract.ValidateCheckpointReceipt(receipt, o.request, o.digest); err != nil {
			return classifyStoredResourceRecordError(err)
		}
		if err := tx.Verify(checkpointReceiptName, receipt); err != nil {
			return err
		}
		return o.requireReceiptCandidateTx(tx, receipt, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch))
	})
	return receipt, resultErr
}

func (o *Operation) requireCheckpointCreation() error {
	if o.request.Kind != contract.OperationCheckpoint {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (o *Operation) readCheckpointCandidateTx(tx *taskdir.ControlTransaction) (contract.CheckpointCandidate, error) {
	if err := o.requireCheckpointCreation(); err != nil {
		return contract.CheckpointCandidate{}, err
	}
	var candidate contract.CheckpointCandidate
	if err := tx.Read(checkpointCandidateName, &candidate); err != nil {
		return contract.CheckpointCandidate{}, classifyResourceRecordReadError(err)
	}
	if err := contract.ValidateCheckpointCandidate(candidate, o.request, o.digest); err != nil {
		return contract.CheckpointCandidate{}, classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(checkpointCandidateName, candidate); err != nil {
		return contract.CheckpointCandidate{}, err
	}
	return candidate, nil
}

// requireReceiptCandidateTx binds a receipt's candidate echo to the stored
// candidate. A missing stored candidate is an evidence fault; a different one
// is reported with the caller's mismatch classification.
func (o *Operation) requireReceiptCandidateTx(tx *taskdir.ControlTransaction, receipt contract.CheckpointReceipt, mismatch error) error {
	if receipt.Candidate == nil {
		return nil
	}
	stored, err := o.readCheckpointCandidateTx(tx)
	if err != nil {
		return requireResourceRecord(err)
	}
	if stored != *receipt.Candidate {
		return mismatch
	}
	return nil
}
