package supervisor

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func ValidateManagedRetryRunnerContext(ctx context.Context, td *taskdir.TaskDir, meta *task.MetaRecord, runner string, target RunnerRepairTarget) error {
	if td == nil || meta == nil {
		return task.ErrEvidenceFault
	}
	root := filepath.Clean(filepath.Dir(filepath.Dir(td.Dir)))
	ownership, err := RunnerOwnershipForTaskContext(ctx, root, td, meta)
	if err != nil {
		return err
	}
	if ownership != task.RunnerOwnershipManaged {
		return task.ErrIdentityMismatch
	}
	if runner == meta.RunnerExecutable && !pueue.IsLegacyStateRunnerPath(root, runner) {
		_, err = installation.ResolveRunner(runner, "")
		return err
	}
	if err = target.ValidateExecutable(runner); err != nil {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	return nil
}
