package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

// The original checkpoint journal owns its removal attempts: references
// removal-attempt-<sequence>.json contiguous from 1, and at most one qualified
// receipt removal-receipt-<sequence>.json per referenced attempt. The removal
// operation's own journal keeps only its request and lifetime; nothing else
// indexes or points at a current attempt.
const (
	checkpointRemovalNamespace     = "removal-"
	checkpointRemovalAttemptPrefix = "removal-attempt-"
	checkpointRemovalReceiptPrefix = "removal-receipt-"
	checkpointRemovalSequenceWidth = 20
)

// ErrCheckpointRemovalHeadChanged reports that another attempt was registered
// after the caller observed the attempt head. The caller's removal request
// stays unreferenced and must never be submitted.
var ErrCheckpointRemovalHeadChanged = fmt.Errorf("%w: checkpoint removal attempt head changed", os.ErrExist)

// RegisterCheckpointRemoval appends removal as the next attempt only when the
// registered head still equals expectedHead (0 before any attempt). An
// attempt already registered for removal is returned with created=false, so a
// registration whose result was lost replays to the same record. The
// transaction only reads and writes this journal's records.
func (o *Operation) RegisterCheckpointRemoval(ctx context.Context, removal *Operation, expectedHead uint64) (attempt contract.CheckpointRemovalReference, created bool, resultErr error) {
	if err := o.requireCheckpointRemoval(removal); err != nil {
		return attempt, false, err
	}
	resultErr = o.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		attempts, err := o.readCheckpointRemovalAttemptsTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, registered := range attempts {
			if registered.OperationID == removal.request.OperationID {
				attempt = registered
				return classifyStoredResourceRecordError(removal.requireRegisteredAttempt(registered))
			}
		}
		if uint64(len(attempts)) != expectedHead {
			return ErrCheckpointRemovalHeadChanged
		}
		attempt, created, err = putCheckpointRemovalAttemptTx(tx, removal, expectedHead)
		return err
	})
	if resultErr != nil {
		return contract.CheckpointRemovalReference{}, false, resultErr
	}
	return attempt, created, nil
}

// CheckpointRemovalAttempts reads the complete registered attempts 1..N.
func (o *Operation) CheckpointRemovalAttempts(ctx context.Context) (attempts []contract.CheckpointRemovalReference, resultErr error) {
	resultErr = o.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		attempts, err = o.readCheckpointRemovalAttemptsTx(ctx, tx)
		return err
	})
	if resultErr != nil {
		return nil, resultErr
	}
	return attempts, nil
}

// RecordCheckpointRemovalReceipt persists the coordinator's qualified
// disposition of one registered attempt after its closure. The receipt must
// close exactly the registered reference of removal. Removed requires this
// checkpoint's candidate; without one the qualified success is
// nothing_to_remove. A failed attempt occupies only its own sequence.
func (o *Operation) RecordCheckpointRemovalReceipt(ctx context.Context, removal *Operation, receipt contract.CheckpointRemovalReceipt) error {
	if err := o.requireCheckpointRemoval(removal); err != nil {
		return err
	}
	if err := removal.requireRegisteredAttempt(receipt.Attempt); err != nil {
		return err
	}
	if err := contract.ValidateCheckpointRemovalReceipt(receipt, receipt.Attempt); err != nil {
		return err
	}
	return o.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := o.requireRegisteredAttemptTx(ctx, tx, receipt.Attempt); err != nil {
			return err
		}
		if receipt.Status == contract.CheckpointRemoved {
			if _, err := o.readCheckpointCandidateTx(tx); task.RecordAbsent(err) {
				return errors.Join(task.ErrIdentityMismatch, err)
			} else if err != nil {
				return err
			}
		}
		name := checkpointRemovalReceiptName(receipt.Attempt.Sequence)
		if _, err := tx.Put(name, receipt); err != nil {
			return err
		}
		return tx.Verify(name, receipt)
	})
}

// CheckpointRemovalReceipt reads the qualified disposition of one registered
// attempt. A missing receipt is absent: the attempt is not yet closed.
func (o *Operation) CheckpointRemovalReceipt(ctx context.Context, attempt contract.CheckpointRemovalReference) (receipt contract.CheckpointRemovalReceipt, resultErr error) {
	resultErr = o.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := o.requireRegisteredAttemptTx(ctx, tx, attempt); err != nil {
			return err
		}
		name := checkpointRemovalReceiptName(attempt.Sequence)
		if err := tx.Read(name, &receipt); err != nil {
			return classifyResourceRecordReadError(err)
		}
		if err := contract.ValidateCheckpointRemovalReceipt(receipt, attempt); err != nil {
			return classifyStoredResourceRecordError(err)
		}
		if err := tx.Verify(name, receipt); err != nil {
			return err
		}
		if receipt.Status != contract.CheckpointRemoved {
			return nil
		}
		_, err := o.readCheckpointCandidateTx(tx)
		return requireResourceRecord(err)
	})
	return receipt, resultErr
}

// requireCheckpointRemoval binds a removal request to this creation: same
// root, subject naming this checkpoint and its exact creation digest.
func (o *Operation) requireCheckpointRemoval(removal *Operation) error {
	if err := o.requireCheckpointCreation(); err != nil {
		return err
	}
	if removal == nil {
		return task.ErrIdentityMismatch
	}
	request := removal.request
	subject := contract.Subject{Kind: contract.SubjectOperation, ID: o.request.OperationID, SHA256: o.digest}
	if request.Kind != contract.OperationCheckpointRemove || request.RootID != o.request.RootID || request.Subject != subject {
		return task.ErrIdentityMismatch
	}
	return nil
}

// requireRegisteredAttempt checks that a reference registers this removal
// operation and its exact request digest.
func (o *Operation) requireRegisteredAttempt(attempt contract.CheckpointRemovalReference) error {
	return contract.ValidateCheckpointRemovalReference(attempt, o.request, o.digest)
}

// putCheckpointRemovalAttemptTx creates the reference after head in the
// original journal's transaction tx, registering removal's exact request.
func putCheckpointRemovalAttemptTx(tx *taskdir.ControlTransaction, removal *Operation, head uint64) (contract.CheckpointRemovalReference, bool, error) {
	sequence, err := contract.NextCheckpointRemovalSequence(head)
	if err != nil {
		return contract.CheckpointRemovalReference{}, false, err
	}
	attempt := contract.CheckpointRemovalReference{
		SchemaVersion: task.SchemaVersion, CheckpointID: removal.request.Subject.ID, Sequence: sequence,
		OperationID: removal.request.OperationID, RequestSHA256: removal.digest,
	}
	if err = removal.requireRegisteredAttempt(attempt); err != nil {
		return contract.CheckpointRemovalReference{}, false, err
	}
	name := checkpointRemovalAttemptName(sequence)
	created, err := tx.Put(name, attempt)
	if err != nil {
		return contract.CheckpointRemovalReference{}, false, err
	}
	return attempt, created, tx.Verify(name, attempt)
}

// requireRegisteredAttemptTx authenticates attempt against the complete
// registered inventory; an unregistered or different attempt is refused.
func (o *Operation) requireRegisteredAttemptTx(ctx context.Context, tx *taskdir.ControlTransaction, attempt contract.CheckpointRemovalReference) error {
	attempts, err := o.readCheckpointRemovalAttemptsTx(ctx, tx)
	if err != nil {
		return err
	}
	if attempt.Sequence == 0 || attempt.Sequence > uint64(len(attempts)) || attempts[attempt.Sequence-1] != attempt {
		return task.ErrIdentityMismatch
	}
	return nil
}

// readCheckpointRemovalAttemptsTx inventories the removal namespace. Every
// family-shaped name must be canonical, attempts must be exactly 1..N, and a
// receipt must close a registered attempt; anything else is an evidence fault.
func (o *Operation) readCheckpointRemovalAttemptsTx(ctx context.Context, tx *taskdir.ControlTransaction) ([]contract.CheckpointRemovalReference, error) {
	if err := o.requireCheckpointCreation(); err != nil {
		return nil, err
	}
	head, receipts, err := checkpointRemovalInventory(ctx, o.control)
	if err != nil {
		return nil, err
	}
	for _, sequence := range receipts {
		if sequence > head {
			return nil, fmt.Errorf("%w: checkpoint removal receipt %d has no registered attempt", task.ErrEvidenceFault, sequence)
		}
	}
	attempts := make([]contract.CheckpointRemovalReference, 0, head)
	for sequence := uint64(1); sequence <= head; sequence++ {
		attempt, readErr := readCheckpointRemovalAttemptTx(tx, sequence)
		if readErr != nil {
			return nil, requireResourceRecord(readErr)
		}
		attempts = append(attempts, attempt)
	}
	if err = contract.ValidateCheckpointRemovalReferences(o.request.OperationID, attempts); err != nil {
		return nil, classifyStoredResourceRecordError(err)
	}
	return attempts, nil
}

func readCheckpointRemovalAttemptTx(tx *taskdir.ControlTransaction, sequence uint64) (contract.CheckpointRemovalReference, error) {
	name := checkpointRemovalAttemptName(sequence)
	var attempt contract.CheckpointRemovalReference
	if err := tx.Read(name, &attempt); err != nil {
		return contract.CheckpointRemovalReference{}, classifyResourceRecordReadError(err)
	}
	if attempt.Sequence != sequence {
		return contract.CheckpointRemovalReference{}, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	if err := tx.Verify(name, attempt); err != nil {
		return contract.CheckpointRemovalReference{}, err
	}
	return attempt, nil
}

// checkpointRemovalInventory returns the number of contiguous attempts and the
// receipt sequences present.
func checkpointRemovalInventory(ctx context.Context, control *taskdir.ControlDir) (uint64, []uint64, error) {
	names, err := control.RecordNamesContext(ctx)
	if err != nil {
		return 0, nil, err
	}
	present := make(map[uint64]bool)
	var receipts []uint64
	for _, name := range names {
		if !strings.HasPrefix(name, checkpointRemovalNamespace) {
			continue
		}
		receipt, sequence, parseErr := parseCheckpointRemovalName(name)
		if parseErr != nil {
			return 0, nil, parseErr
		}
		if receipt {
			receipts = append(receipts, sequence)
		} else {
			present[sequence] = true
		}
	}
	head := uint64(len(present))
	for sequence := range present {
		if sequence > head {
			return 0, nil, fmt.Errorf("%w: checkpoint removal attempt %d is not contiguous", task.ErrEvidenceFault, sequence)
		}
	}
	return head, receipts, nil
}

func parseCheckpointRemovalName(name string) (receipt bool, sequence uint64, err error) {
	prefix := checkpointRemovalAttemptPrefix
	if strings.HasPrefix(name, checkpointRemovalReceiptPrefix) {
		prefix, receipt = checkpointRemovalReceiptPrefix, true
	} else if !strings.HasPrefix(name, checkpointRemovalAttemptPrefix) {
		return false, 0, fmt.Errorf("%w: unknown checkpoint removal record %q", task.ErrEvidenceFault, name)
	}
	stem, found := strings.CutSuffix(strings.TrimPrefix(name, prefix), ".json")
	if !found || len(stem) != checkpointRemovalSequenceWidth {
		return false, 0, fmt.Errorf("%w: malformed checkpoint removal record %q", task.ErrEvidenceFault, name)
	}
	sequence, err = strconv.ParseUint(stem, 10, 64)
	if err != nil || sequence == 0 || checkpointRemovalName(prefix, sequence) != name {
		return false, 0, fmt.Errorf("%w: malformed checkpoint removal record %q", task.ErrEvidenceFault, name)
	}
	return receipt, sequence, nil
}

func checkpointRemovalAttemptName(sequence uint64) string {
	return checkpointRemovalName(checkpointRemovalAttemptPrefix, sequence)
}

func checkpointRemovalReceiptName(sequence uint64) string {
	return checkpointRemovalName(checkpointRemovalReceiptPrefix, sequence)
}

func checkpointRemovalName(prefix string, sequence uint64) string {
	return fmt.Sprintf("%s%0*d.json", prefix, checkpointRemovalSequenceWidth, sequence)
}
