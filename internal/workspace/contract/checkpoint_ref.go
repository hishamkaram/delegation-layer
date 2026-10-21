package contract

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// CheckpointRef is the one canonical owned ref for a checkpoint commit:
// refs/delegate/<rootID>/checkpoints/<checkpointID>. Every consumer formats
// or checks the name here rather than assembling it locally.
func CheckpointRef(rootID, checkpointID string) (string, error) {
	if err := errors.Join(task.ValidateRootID(rootID), task.ValidateTaskID(checkpointID)); err != nil {
		return "", err
	}
	return "refs/delegate/" + rootID + "/checkpoints/" + checkpointID, nil
}

// ValidateCheckpointRef accepts only the exact canonical ref for this root and
// checkpoint, never another owned namespace.
func ValidateCheckpointRef(ref, rootID, checkpointID string) error {
	want, err := CheckpointRef(rootID, checkpointID)
	if err != nil {
		return err
	}
	if ref != want {
		return fmt.Errorf("%w: checkpoint ref %q", task.ErrIdentityMismatch, ref)
	}
	return nil
}
