package supervisor

import (
	"context"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func managedRunnerExecutable(root, path, ownership string) bool {
	switch ownership {
	case task.RunnerOwnershipManaged:
		return true
	case task.RunnerOwnershipCustom:
		return false
	}
	return pueue.IsLegacyStateRunnerPath(root, filepath.Clean(path))
}

// ManagedInspectionBinding classifies the saved worker using explicit ownership
// or the legacy state runner path. The inspection journal owns upgrade authority.
func ManagedInspectionBinding(root string, binding inspection.Binding) bool {
	return managedRunnerExecutable(root, binding.WorkerExecutable, binding.RunnerOwnership)
}

// ManagedInspectionWorkerExecutable reads persisted ownership, not image bytes.
// The canonical operation and its upgrade journal retain worker authority.
func ManagedInspectionWorkerExecutable(_ string, binding inspection.Binding) bool {
	return binding.RunnerOwnership == task.RunnerOwnershipManaged &&
		filepath.IsAbs(binding.WorkerExecutable) && filepath.Clean(binding.WorkerExecutable) == binding.WorkerExecutable
}

func PersistedRunnerOwnership(root string, meta *task.MetaRecord) string {
	if meta == nil {
		return ""
	}
	if meta.RunnerOwnership == task.RunnerOwnershipManaged || meta.RunnerOwnership == task.RunnerOwnershipCustom {
		return meta.RunnerOwnership
	}
	if managedRunnerExecutable(root, meta.RunnerExecutable, "") {
		return task.RunnerOwnershipManaged
	}
	return task.RunnerOwnershipCustom
}

func RunnerOwnershipForTask(root string, td *taskdir.TaskDir, meta *task.MetaRecord) (string, error) {
	return RunnerOwnershipForTaskContext(context.Background(), root, td, meta)
}

func RunnerOwnershipForTaskContext(ctx context.Context, root string, td *taskdir.TaskDir, meta *task.MetaRecord) (string, error) {
	if meta == nil || meta.RunnerOwnership != "" || td == nil {
		return PersistedRunnerOwnership(root, meta), nil
	}
	migrated, err := td.HasManagedRunnerMigrationContext(ctx)
	if err != nil {
		return "", err
	}
	if migrated {
		return task.RunnerOwnershipManaged, nil
	}
	return PersistedRunnerOwnership(root, meta), nil
}
