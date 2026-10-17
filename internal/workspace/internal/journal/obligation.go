package journal

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

func (r *Resource) RecordCaptureObligation(ctx context.Context, reservation contract.Reservation, subject contract.Subject) error {
	return r.RecordCaptureObligationWithBudget(ctx, reservation, subject, r.request.BudgetNanos)
}

func (r *Resource) RecordCaptureObligationWithBudget(ctx context.Context, reservation contract.Reservation, subject contract.Subject, budgetNanos int64) error {
	predecessor := subject
	return r.recordCaptureObligation(ctx, reservation, subject, &predecessor, budgetNanos)
}

func (r *Resource) recordCaptureObligation(ctx context.Context, reservation contract.Reservation, subject contract.Subject, predecessor *contract.Subject, budgetNanos int64) error {
	if budgetNanos <= 0 {
		return task.ErrIdentityMismatch
	}
	if err := errors.Join(r.validateReservation(reservation, reservation.Sequence), contract.ValidateSubject(subject)); err != nil {
		return err
	}
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := tx.Verify(reservationName(reservation.Sequence), reservation); err != nil {
			return err
		}
		var closed contract.ReservationClosed
		if err := tx.Read(reservationClosedName(reservation.Sequence), &closed); !task.RecordAbsent(err) {
			return errors.Join(contract.ErrBusy, classifyResourceRecordReadError(err))
		}
		_, err := tx.Put(obligationName(reservation.Sequence), contract.CaptureObligation{SchemaVersion: task.SchemaVersion, WorkspaceID: reservation.WorkspaceID, Sequence: reservation.Sequence, Subject: subject, Predecessor: predecessor, BudgetNanos: budgetNanos})
		return err
	})
}

func (r *Resource) CaptureObligation(ctx context.Context, sequence uint64) (record contract.CaptureObligation, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		name := obligationName(sequence)
		if err := tx.Read(name, &record); err != nil {
			return classifyResourceRecordReadError(err)
		}
		if err := validateCaptureObligationRecord(record, r.request.WorkspaceID, sequence); err != nil {
			return classifyStoredResourceRecordError(err)
		}
		return tx.Verify(name, record)
	})
	return record, resultErr
}

const (
	capturePurposeRecordPrefix    = "capture-purpose-"
	captureObligationRecordPrefix = "capture-obligation-"
)

// CapturePurpose returns the canonical operation-purpose record name for one
// reservation sequence. The application uses this name to associate the
// single capture operation with its durable obligation.
func CapturePurpose(sequence uint64) string {
	return fmt.Sprintf("%s%020d.json", capturePurposeRecordPrefix, sequence)
}

func obligationName(sequence uint64) string {
	return fmt.Sprintf("%s%020d.json", captureObligationRecordPrefix, sequence)
}

func validateCaptureObligationRecord(record contract.CaptureObligation, workspaceID string, sequence uint64) error {
	if record.SchemaVersion != task.SchemaVersion || record.WorkspaceID != workspaceID || record.Sequence != sequence || record.BudgetNanos <= 0 {
		return task.ErrIdentityMismatch
	}
	if err := task.ValidateTaskID(record.WorkspaceID); err != nil {
		return err
	}
	if err := contract.ValidateSubject(record.Subject); err != nil {
		return err
	}
	if record.Predecessor != nil {
		return contract.ValidateSubject(*record.Predecessor)
	}
	return nil
}

// AssociateOperation elects one immutable operation for an already-authorized
// purpose. A losing candidate has never been submitted. Reopening a purpose
// returns its original deadline, including when that deadline has expired.
func (r *Resource) AssociateOperation(ctx context.Context, purpose string, proposed contract.OperationRequest) (winnerID string, resultErr error) {
	if proposed.WorkspaceID != r.request.WorkspaceID {
		return "", task.ErrIdentityMismatch
	}
	proposedReference := contract.OperationReference{SchemaVersion: task.SchemaVersion, WorkspaceID: proposed.WorkspaceID, OperationID: proposed.OperationID, Kind: proposed.Kind}
	if err := validateOperationReference(proposedReference, r.request.WorkspaceID, proposed.OperationID); err != nil {
		return "", err
	}
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var winner contract.OperationReference
		readErr := tx.Read(purpose, &winner)
		if task.RecordAbsent(readErr) {
			winner = proposedReference
			if _, err := tx.Put(purpose, winner); err != nil {
				return err
			}
		} else if readErr != nil {
			return classifyResourceRecordReadError(readErr)
		}
		if err := validateOperationReference(winner, r.request.WorkspaceID, winner.OperationID); err != nil {
			return classifyStoredResourceRecordError(err)
		}
		if winner.Kind != proposed.Kind {
			return task.ErrIdentityMismatch
		}
		if err := tx.Verify(purpose, winner); err != nil {
			return err
		}
		winnerID = winner.OperationID
		return nil
	})
	return winnerID, resultErr
}

func (r *Resource) AssociatedOperation(ctx context.Context, purpose string) (reference contract.OperationReference, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := tx.Read(purpose, &reference); err != nil {
			return classifyResourceRecordReadError(err)
		}
		if err := validateOperationReference(reference, r.request.WorkspaceID, reference.OperationID); err != nil {
			return classifyStoredResourceRecordError(err)
		}
		return tx.Verify(purpose, reference)
	})
	return reference, resultErr
}

// AdmissionAborted prevents a dead admission owner from creating a task on a
// later replay. The coordinator holds AcquireUseOwner while proving not-launched.
func (r *Resource) RecordAdmissionAborted(ctx context.Context, reservation contract.Reservation) error {
	return r.CloseReservation(ctx, reservation, "aborted-not-launched", "")
}

// OperationAbsent is deliberately narrow: unreadable journals are not absent.
func OperationAbsent(err error) bool { return task.RecordAbsent(err) }
