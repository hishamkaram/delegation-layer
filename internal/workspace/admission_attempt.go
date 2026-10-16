package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// AdmissionAttempt is a read-only projection of an existing reservation. The
// resource's monotonic sequence owns attempt identity; no attempt journal or
// mutable current-attempt pointer is introduced.
type AdmissionAttempt struct {
	RootID        string `json:"root_id"`
	WorkspaceID   string `json:"workspace_id"`
	TaskID        string `json:"task_id"`
	RequestSHA256 string `json:"request_sha256"`
	Sequence      uint64 `json:"sequence"`
}

func (r *Resource) ResolveAdmissionAttempt(ctx context.Context, sequence uint64, subject Subject) (attempt AdmissionAttempt, resultErr error) {
	if sequence == 0 || subject.Kind != SubjectAdmission {
		return attempt, task.ErrIdentityMismatch
	}
	if err := validateSubject(subject); err != nil {
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
		attempt = AdmissionAttempt{
			RootID: r.request.RootID, WorkspaceID: reservation.WorkspaceID,
			TaskID: subject.ID, RequestSHA256: subject.SHA256, Sequence: reservation.Sequence,
		}
		return attempt.validate()
	})
	return attempt, resultErr
}

func (attempt AdmissionAttempt) InspectionOperationID() (string, error) {
	if err := attempt.validate(); err != nil {
		return "", err
	}
	data, err := task.MarshalCanonical(attempt)
	if err != nil {
		return "", err
	}
	digest := task.ComputeSHA256(append([]byte("delegation-layer:workspace-admission-inspection:v1\n"), data...))
	return digest[:32], nil
}

func (attempt AdmissionAttempt) ProvisionPurpose() (string, error) {
	if err := attempt.validate(); err != nil {
		return "", err
	}
	return fmt.Sprintf("provision-%020d.json", attempt.Sequence), nil
}

func (attempt AdmissionAttempt) validate() error {
	if attempt.Sequence == 0 {
		return task.ErrIdentityMismatch
	}
	return errors.Join(task.ValidateRootID(attempt.RootID), task.ValidateTaskID(attempt.WorkspaceID),
		task.ValidateTaskID(attempt.TaskID), task.ValidateSHA256(attempt.RequestSHA256))
}
