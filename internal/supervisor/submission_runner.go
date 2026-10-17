package supervisor

import (
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func ValidateManagedRetryRunner(td *taskdir.TaskDir, meta *task.MetaRecord, runner string) error {
	if td == nil || meta == nil {
		return task.ErrEvidenceFault
	}
	root := filepath.Clean(filepath.Dir(filepath.Dir(td.Dir)))
	ownership, err := RunnerOwnershipForTask(root, td, meta)
	if err != nil {
		return err
	}
	if ownership != task.RunnerOwnershipManaged || !CurrentManagedRunnerExecutable(root, runner) {
		return task.ErrIdentityMismatch
	}
	return nil
}
