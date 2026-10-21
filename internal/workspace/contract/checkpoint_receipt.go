package contract

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Historical checkpoint creation dispositions and unavailable reasons.
const (
	CheckpointCheckpointed = "checkpointed"
	CheckpointUnavailable  = "unavailable"

	CheckpointFailed            = "checkpoint_failed"
	CheckpointNotLaunched       = "checkpoint_not_launched"
	CheckpointResultUnavailable = "checkpoint_result_unavailable"
)

// CheckpointReceipt is the qualified, immutable creation disposition sealed
// after canonical lifetime closure. It records history only: whether the
// checkpoint is still usable is derived from removal references, never stored
// here. Candidate is present exactly when checkpointed; an unavailable receipt
// does not deny that a durable candidate exists.
type CheckpointReceipt struct {
	LifetimeClosure   task.LifetimeClosureKind `json:"lifetime_closure,omitempty"`
	SchemaVersion     int                      `json:"schema_version"`
	OperationID       string                   `json:"operation_id"`
	RequestSHA256     string                   `json:"request_sha256"`
	Status            string                   `json:"status"`
	ReasonCode        string                   `json:"reason_code,omitempty"`
	Candidate         *CheckpointCandidate     `json:"candidate,omitempty"`
	TerminationSHA256 string                   `json:"termination_sha256,omitempty"`
	WorkerWaitSHA256  string                   `json:"worker_wait_sha256,omitempty"`
	UnlaunchedSHA256  string                   `json:"unlaunched_sha256,omitempty"`
}

// Closure returns the receipt's canonical closure evidence.
func (r CheckpointReceipt) Closure() ClosureEvidence {
	return ClosureEvidence{Kind: r.LifetimeClosure, TerminationSHA256: r.TerminationSHA256, WorkerWaitSHA256: r.WorkerWaitSHA256, UnlaunchedSHA256: r.UnlaunchedSHA256}
}

// ValidateCheckpointReceipt binds a creation receipt to its exact request and
// digest, closure evidence and disposition.
func ValidateCheckpointReceipt(receipt CheckpointReceipt, request OperationRequest, requestSHA256 string) error {
	if request.Kind != OperationCheckpoint || receipt.SchemaVersion != task.SchemaVersion ||
		receipt.OperationID != request.OperationID || receipt.RequestSHA256 != requestSHA256 {
		return task.ErrIdentityMismatch
	}
	workerMayHaveRun, err := ValidateClosureEvidence(receipt.Closure())
	if err = errors.Join(err, task.ValidateSHA256(receipt.RequestSHA256)); err != nil {
		return err
	}
	switch receipt.Status {
	case CheckpointCheckpointed:
		if receipt.Candidate == nil || receipt.ReasonCode != "" {
			return task.ErrIdentityMismatch
		}
		if err = validateCheckpointSuccessClosure(receipt.Closure()); err != nil {
			return err
		}
		return ValidateCheckpointCandidate(*receipt.Candidate, request, requestSHA256)
	case CheckpointUnavailable:
		if receipt.Candidate != nil {
			return task.ErrIdentityMismatch
		}
		return validateUnavailableReason(receipt.ReasonCode, CheckpointNotLaunched, workerMayHaveRun, CheckpointFailed, CheckpointResultUnavailable)
	default:
		return task.ErrIdentityMismatch
	}
}
