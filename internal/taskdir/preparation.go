package taskdir

import (
	"errors"
	"reflect"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// NormalizeTaskRecord validates the current execution directories and applies
// request defaults before immutable hashing. Failure leaves the caller unchanged.
func NormalizeTaskRecord(root string, record *task.TaskRecord) error {
	if record == nil {
		return errors.New("nil task record")
	}
	normalized := *record
	if err := task.NormalizeRequestedConfig(&normalized); err != nil {
		return err
	}
	_, cwd, err := config.ValidateDirectories(root, normalized.CanonicalCwd)
	if err != nil {
		return err
	}
	normalized.CanonicalCwd = cwd
	*record = normalized
	return nil
}

// Saved evidence can outlive its workspace. Probe live directories only before
// issuing fresh execution authority, after checking immutable admission guards.
func (td *TaskDir) validateExecutionDirectories(record *task.TaskRecord) error {
	normalized := *record
	if err := NormalizeTaskRecord(td.store.Root, &normalized); err != nil {
		return err
	}
	if !reflect.DeepEqual(*record, normalized) {
		return task.ErrIdentityMismatch
	}
	return nil
}
