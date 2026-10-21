package contract

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Checkpoint selection kinds. There is no default: creation names one.
const (
	CheckpointOrdinary = "ordinary"
	CheckpointManaged  = "managed"
)

// CheckpointSelection chooses the checkout a checkpoint observes. An ordinary
// selection carries the caller's canonical directory and its physical identity.
// A managed selection carries nothing else: the request's WorkspaceID is the
// only workspace identity authority.
type CheckpointSelection struct {
	Kind        string             `json:"kind"`
	Cwd         string             `json:"cwd,omitempty"`
	CwdIdentity *DirectoryIdentity `json:"cwd_identity,omitempty"`
}

// validateOperationScope binds each operation kind to its workspace and
// checkpoint selection. Earlier kinds keep their required workspace identity.
func validateOperationScope(request OperationRequest) error {
	switch request.Kind {
	case OperationCheckpoint:
		return validateCheckpointSelection(request.Checkpoint, request.WorkspaceID)
	case OperationCheckpointRemove:
		if request.Checkpoint != nil || request.WorkspaceID != "" || request.Subject.Kind != SubjectOperation {
			return task.ErrIdentityMismatch
		}
		return nil
	default:
		if request.Checkpoint != nil {
			return task.ErrIdentityMismatch
		}
		return task.ValidateTaskID(request.WorkspaceID)
	}
}

func validateCheckpointSelection(selection *CheckpointSelection, workspaceID string) error {
	if selection == nil {
		return task.ErrIdentityMismatch
	}
	switch selection.Kind {
	case CheckpointOrdinary:
		if workspaceID != "" || selection.CwdIdentity == nil || selection.CwdIdentity.Inode == 0 {
			return task.ErrIdentityMismatch
		}
		return ValidateAbsoluteCleanPath(selection.Cwd)
	case CheckpointManaged:
		if selection.Cwd != "" || selection.CwdIdentity != nil {
			return task.ErrIdentityMismatch
		}
		return task.ValidateTaskID(workspaceID)
	default:
		return task.ErrIdentityMismatch
	}
}
