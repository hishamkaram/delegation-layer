package journal

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

func (r *Resource) ResolveAdmissionAttempt(ctx context.Context, sequence uint64, subject contract.Subject) (attempt contract.AdmissionAttempt, resultErr error) {
	if sequence == 0 || subject.Kind != contract.SubjectAdmission {
		return attempt, task.ErrIdentityMismatch
	}
	if err := contract.ValidateSubject(subject); err != nil {
		return attempt, err
	}
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		reservation, err := readReservationTx(tx, r.request.WorkspaceID, sequence)
		if err != nil {
			return requireResourceRecord(err)
		}
		if reservation.Subject != subject {
			return task.ErrIdentityMismatch
		}
		attempt = contract.AdmissionAttempt{
			RootID: r.request.RootID, WorkspaceID: reservation.WorkspaceID,
			TaskID: subject.ID, RequestSHA256: subject.SHA256, Sequence: reservation.Sequence,
		}
		return attempt.Validate()
	})
	return attempt, resultErr
}
