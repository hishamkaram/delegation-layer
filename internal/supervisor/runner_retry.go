package supervisor

import (
	"context"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func RefreshManagedRetryRunnerContext(ctx context.Context, selection AdmissionSelection, defaultRunner string, root string, td *taskdir.TaskDir, meta *task.MetaRecord) (AdmissionSelection, bool, error) {
	ownership, err := RunnerOwnershipForTaskContext(ctx, root, td, meta)
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
	recorded, err := ContinuationRunner(root, meta.RunnerExecutable, ownership)
	if err != nil {
		return selection, false, err
	}
	runner, err := ResolveRunner(recorded, defaultRunner)
	if err != nil {
		return selection, false, fmt.Errorf("resolve current managed task runner: %w", err)
	}
	if meta.RunnerOwnership == "" {
		if err = RecordManagedRunnerMigration(ctx, root, td, meta); err != nil {
			return selection, false, fmt.Errorf("record managed task runner migration: %w", err)
		}
	}
	selection.Runner = runner
	selection.RunnerOwnership = task.RunnerOwnershipManaged
	return selection, true, nil
}

func RepairQueueForTask(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, options AdmissionOptions) error {
	return repairQueueForTask(ctx, store, binding, meta, TaskSupervisorObservationOptions{Control: options.Control, Lookup: options.Lookup(), RunnerExecutable: options.RunnerExecutable, Routing: options.Routing})
}

// Submitted recovery selects its committed submission, rather than falling
// back to the task's earlier preparation slot for the same logical binding.
func RepairQueueForSubmittedTask(ctx context.Context, store *taskdir.Store, submission *task.SubmitRecord, meta *task.MetaRecord, options AdmissionOptions) error {
	if submission == nil {
		return task.ErrEvidenceFault
	}
	source, err := NewSavedBindingSource(submission.RootID, infrastructure.SavedBindingTaskSubmission, submission.TaskID, "", submission.Supervisor)
	if err != nil {
		return err
	}
	return repairQueueForTask(ctx, store, submission.Supervisor, meta, TaskSupervisorObservationOptions{Control: options.Control, Lookup: options.Lookup(), RunnerExecutable: options.RunnerExecutable, Routing: options.Routing, Source: source})
}

func repairQueueForTask(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, configuration TaskSupervisorObservationOptions) (resultErr error) {
	client, err := PrepareTaskSupervisorWithRouting(ctx, store, binding, meta, configuration)
	if err != nil || client == nil {
		return ClosePrivateAdmissionTransition(client, err)
	}
	defer func() { CloseMaintenance(client, resultErr) }()
	return nil
}
