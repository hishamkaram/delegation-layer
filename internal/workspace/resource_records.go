package workspace

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// resourceRecord is the closed set of values that Resource may decode from
// its journal. Each caller performs the family-specific structural checks
// after this exact-byte read and verification.
type resourceRecord interface {
	ResourceRequest | Registration | Reservation | ReservationClosed |
		ReleaseRecord | OperationReference | Source | Checkout |
		CaptureCandidate | CaptureReceipt | CaptureObligation | ForgetIntent |
		ForgottenRecord | RemovedRecord
}

func readResourceRecord[T resourceRecord](ctx context.Context, control *taskdir.ControlDir, name string) (record T, resultErr error) {
	resultErr = control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := tx.Read(name, &record); err != nil {
			return classifyResourceRecordReadError(err)
		}
		return tx.Verify(name, record)
	})
	return record, resultErr
}

func classifyResourceRecordReadError(err error) error {
	if err == nil || task.RecordAbsent(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, task.ErrIdentityMismatch) || errors.Is(err, task.ErrEvidenceFault) {
		return err
	}
	return errors.Join(task.ErrEvidenceFault, err)
}

func classifyStoredResourceRecordError(err error) error {
	if err == nil || task.RecordAbsent(err) || errors.Is(err, task.ErrEvidenceFault) {
		return err
	}
	return errors.Join(task.ErrEvidenceFault, err)
}

func requireResourceRecord(err error) error {
	if task.RecordAbsent(err) {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	return classifyResourceRecordReadError(err)
}
