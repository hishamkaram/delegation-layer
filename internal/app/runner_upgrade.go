package app

import (
	"context"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func refreshManagedRetryRunner(a Arguments, deps Dependencies, root string, store *taskdir.Store, td *taskdir.TaskDir, meta *task.MetaRecord) (Arguments, bool, error) {
	selection, upgraded, err := supervisorcore.RefreshManagedRetryRunner(supervisorAdmissionSelection(a), deps.RunnerExecutable, root, store, td, meta)
	a.Runner, a.runnerOwnership = selection.Runner, selection.RunnerOwnership
	return a, upgraded, err
}

func repairPrivateQueueForTask(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, meta *task.MetaRecord, deps Dependencies) error {
	return supervisorcore.RepairQueueForTask(ctx, store, binding, meta, supervisorAdmissionOptions(deps, deps.SupervisorOptions))
}
