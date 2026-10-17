package supervisor

import (
	"context"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func RefreshManagedRetryRunner(selection AdmissionSelection, defaultRunner string, root string, store *taskdir.Store, td *taskdir.TaskDir, meta *task.MetaRecord) (AdmissionSelection, bool, error) {
	ownership, err := RunnerOwnershipForTask(root, td, meta)
	if err != nil {
		return selection, false, err
	}
	if meta == nil || ownership != task.RunnerOwnershipManaged {
		return selection, false, nil
	}
	if selection.Runner != "" {
		if selection.Runner != meta.RunnerExecutable {
			return selection, false, task.ErrIdentityMismatch
		}
		return selection, false, nil
	}
	pinnedRunner, pinned, err := LegacyInspectionRetryRunner(store, td, meta)
	if err != nil {
		return selection, false, err
	}
	if pinned {
		selection.Runner = pinnedRunner
		return selection, false, nil
	}
	runner, err := ResolveRunner("", defaultRunner)
	if err != nil {
		return selection, false, fmt.Errorf("resolve current managed task runner: %w", err)
	}
	runner, err = InstallStateRunner(root, runner)
	if err != nil {
		return selection, false, fmt.Errorf("install current managed task runner: %w", err)
	}
	if meta.RunnerOwnership == "" {
		if err = RecordManagedRunnerMigration(context.Background(), root, td, meta); err != nil {
			return selection, false, fmt.Errorf("record managed task runner migration: %w", err)
		}
	}
	selection.Runner = runner
	selection.RunnerOwnership = task.RunnerOwnershipManaged
	return selection, true, nil
}

func RepairQueueForTask(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, options AdmissionOptions) (resultErr error) {
	client, err := PrepareTaskSupervisor(ctx, store, binding, meta, options.Control, options.InitialExecutable, options.RunnerExecutable)
	if err != nil || client == nil {
		return err
	}
	defer func() { CloseMaintenance(client, resultErr) }()
	return nil
}
