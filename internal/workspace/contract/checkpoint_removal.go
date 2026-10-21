package contract

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Terminal checkpoint removal dispositions and unavailable reasons. Removed
// and nothing-to-remove are verified outcomes in the pinned common directory;
// a missing or replaced common directory is unavailable, never removed.
const (
	CheckpointRemoved         = "removed"
	CheckpointNothingToRemove = "nothing_to_remove"

	CheckpointRemovalFailed            = "checkpoint_removal_failed"
	CheckpointRemovalNotLaunched       = "checkpoint_removal_not_launched"
	CheckpointRemovalResultUnavailable = "checkpoint_removal_result_unavailable"
)

// CheckpointRemovalReference registers one finite removal attempt of a
// checkpoint. Sequences are contiguous from 1 for that checkpoint. The
// removal request's subject names the checkpoint and its creation digest.
type CheckpointRemovalReference struct {
	SchemaVersion int    `json:"schema_version"`
	CheckpointID  string `json:"checkpoint_id"`
	Sequence      uint64 `json:"sequence"`
	OperationID   string `json:"operation_id"`
	RequestSHA256 string `json:"request_sha256"`
}

// CheckpointRemovalReceipt is the qualified terminal disposition of exactly
// one authenticated removal attempt.
type CheckpointRemovalReceipt struct {
	LifetimeClosure   task.LifetimeClosureKind   `json:"lifetime_closure,omitempty"`
	SchemaVersion     int                        `json:"schema_version"`
	Attempt           CheckpointRemovalReference `json:"attempt"`
	Status            string                     `json:"status"`
	ReasonCode        string                     `json:"reason_code,omitempty"`
	TerminationSHA256 string                     `json:"termination_sha256,omitempty"`
	WorkerWaitSHA256  string                     `json:"worker_wait_sha256,omitempty"`
	UnlaunchedSHA256  string                     `json:"unlaunched_sha256,omitempty"`
}

// Closure returns the receipt's canonical closure evidence.
func (r CheckpointRemovalReceipt) Closure() ClosureEvidence {
	return ClosureEvidence{Kind: r.LifetimeClosure, TerminationSHA256: r.TerminationSHA256, WorkerWaitSHA256: r.WorkerWaitSHA256, UnlaunchedSHA256: r.UnlaunchedSHA256}
}

// ValidateCheckpointRemovalReference binds a registered attempt to its exact
// removal request and digest.
func ValidateCheckpointRemovalReference(reference CheckpointRemovalReference, request OperationRequest, requestSHA256 string) error {
	if request.Kind != OperationCheckpointRemove || request.Subject.Kind != SubjectOperation || reference.CheckpointID != request.Subject.ID ||
		reference.OperationID != request.OperationID || reference.RequestSHA256 != requestSHA256 {
		return task.ErrIdentityMismatch
	}
	return validateRemovalReferenceShape(reference)
}

// ValidateCheckpointRemovalReferences accepts one checkpoint's complete
// registered attempts in order: sequences exactly 1..N with distinct
// operations. An invalid checkpoint ID refuses even an empty list; a gap,
// duplicate, foreign checkpoint or malformed entry refuses.
func ValidateCheckpointRemovalReferences(checkpointID string, references []CheckpointRemovalReference) error {
	if err := task.ValidateTaskID(checkpointID); err != nil {
		return err
	}
	operations := make(map[string]bool, len(references))
	for index, reference := range references {
		if reference.CheckpointID != checkpointID || reference.Sequence != uint64(index)+1 || operations[reference.OperationID] {
			return task.ErrIdentityMismatch
		}
		if err := validateRemovalReferenceShape(reference); err != nil {
			return err
		}
		operations[reference.OperationID] = true
	}
	return nil
}

// NextCheckpointRemovalSequence returns the sequence after the last
// registered attempt, or 1 when none exists, and refuses overflow.
func NextCheckpointRemovalSequence(last uint64) (uint64, error) {
	if last == ^uint64(0) {
		return 0, task.ErrIdentityMismatch
	}
	return last + 1, nil
}

// ValidateCheckpointRemovalReceipt binds a terminal receipt to the
// authenticated attempt it closes, its closure evidence and disposition.
func ValidateCheckpointRemovalReceipt(receipt CheckpointRemovalReceipt, attempt CheckpointRemovalReference) error {
	if receipt.SchemaVersion != task.SchemaVersion || receipt.Attempt != attempt {
		return task.ErrIdentityMismatch
	}
	workerMayHaveRun, err := ValidateClosureEvidence(receipt.Closure())
	if err = errors.Join(err, validateRemovalReferenceShape(attempt)); err != nil {
		return err
	}
	switch receipt.Status {
	case CheckpointRemoved, CheckpointNothingToRemove:
		if receipt.ReasonCode != "" {
			return task.ErrIdentityMismatch
		}
		return validateCheckpointSuccessClosure(receipt.Closure())
	case CheckpointUnavailable:
		return validateUnavailableReason(receipt.ReasonCode, CheckpointRemovalNotLaunched, workerMayHaveRun, CheckpointRemovalFailed, CheckpointRemovalResultUnavailable)
	default:
		return task.ErrIdentityMismatch
	}
}

func validateRemovalReferenceShape(reference CheckpointRemovalReference) error {
	if reference.SchemaVersion != task.SchemaVersion || reference.Sequence == 0 {
		return task.ErrIdentityMismatch
	}
	return errors.Join(task.ValidateTaskID(reference.CheckpointID), task.ValidateTaskID(reference.OperationID), task.ValidateSHA256(reference.RequestSHA256))
}
