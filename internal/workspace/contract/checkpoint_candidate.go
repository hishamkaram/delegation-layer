package contract

import (
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// CheckpointTarget is the checkout a checkpoint resolved. Source is the
// existing Git provenance with its pinned common-directory identity.
// CheckoutRoot is the observed worktree root and AdminDir the actual Git
// administrative directory holding its index. An ordinary target is its own
// source; it is never recorded as a managed Checkout. This is shape policy
// only: Git topology checks stay with their runtime owner.
//
// An ordinary target must resolve the selected directory exactly. That relies
// on the selection Cwd already being canonical (actual directory-entry
// spelling), as established by the application's path canonicalization.
type CheckpointTarget struct {
	Source       Source `json:"source"`
	CheckoutRoot string `json:"checkout_root"`
	AdminDir     string `json:"admin_dir"`
}

// CheckpointCandidate is immutable evidence recorded before the owned ref is
// published. CommitPayload is the exact bounded ASCII commit object body that
// native Git hashed to CommitOID; it is stored once and never reconstructed.
// IndexSHA256 is the semantic real-index identity, as for captures.
type CheckpointCandidate struct {
	SchemaVersion int              `json:"schema_version"`
	OperationID   string           `json:"operation_id"`
	RequestSHA256 string           `json:"request_sha256"`
	Target        CheckpointTarget `json:"target"`
	ObservedHEAD  string           `json:"observed_head"`
	IndexSHA256   string           `json:"index_sha256"`
	TreeOID       string           `json:"tree_oid"`
	CommitOID     string           `json:"commit_oid"`
	SnapshotRef   string           `json:"snapshot_ref"`
	CommitPayload string           `json:"commit_payload"`
}

// ValidateCheckpointCandidate binds a candidate to its exact creation request
// and digest, resolved target, owned ref and stored commit payload. Object
// IDs are checked for format only; native Git owns their computation.
func ValidateCheckpointCandidate(candidate CheckpointCandidate, request OperationRequest, requestSHA256 string) error {
	if request.Kind != OperationCheckpoint || request.Checkpoint == nil || candidate.SchemaVersion != task.SchemaVersion ||
		candidate.OperationID != request.OperationID || candidate.RequestSHA256 != requestSHA256 {
		return task.ErrIdentityMismatch
	}
	if err := errors.Join(task.ValidateSHA256(candidate.RequestSHA256), task.ValidateSHA256(candidate.IndexSHA256),
		validateCheckpointTarget(candidate.Target, *request.Checkpoint),
		ValidateCheckpointRef(candidate.SnapshotRef, request.RootID, candidate.OperationID)); err != nil {
		return err
	}
	for _, oid := range []string{candidate.ObservedHEAD, candidate.TreeOID, candidate.CommitOID} {
		if err := ValidateOID(oid, candidate.Target.Source.ObjectFormat); err != nil {
			return err
		}
	}
	return validateCheckpointPayload(candidate)
}

func validateCheckpointTarget(target CheckpointTarget, selection CheckpointSelection) error {
	source := target.Source
	if err := ValidateSourceShape(source); err != nil {
		return err
	}
	for _, path := range []string{source.Root, source.CommonDir, source.GitDir, target.CheckoutRoot, target.AdminDir} {
		if err := ValidateAbsoluteCleanPath(path); err != nil {
			return err
		}
	}
	if _, pinned := source.CommonDirIdentity(); !pinned {
		return task.ErrIdentityMismatch
	}
	switch selection.Kind {
	case CheckpointOrdinary:
		if target.CheckoutRoot != source.Root || target.AdminDir != source.GitDir ||
			filepath.Join(source.Root, filepath.FromSlash(source.RelativeCwd)) != selection.Cwd {
			return task.ErrIdentityMismatch
		}
		return nil
	case CheckpointManaged:
		if target.CheckoutRoot == source.Root || target.AdminDir == source.CommonDir {
			return task.ErrIdentityMismatch
		}
		return nil
	default:
		return task.ErrIdentityMismatch
	}
}
