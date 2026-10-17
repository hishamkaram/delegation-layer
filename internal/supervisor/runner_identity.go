package supervisor

import (
	"debug/buildinfo"
	"errors"
	"os"
	"path/filepath"
	"strings"

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
	if IsStateRunnerPath(root, filepath.Clean(path)) {
		return true
	}
	info, err := buildinfo.ReadFile(path)
	return err == nil && ManagedRunnerBuildInfo(info)
}

func ManagedInspectionWorkerExecutable(root string, binding inspection.Binding) bool {
	if binding.RunnerOwnership != task.RunnerOwnershipManaged {
		return false
	}
	path := filepath.Clean(binding.WorkerExecutable)
	if !IsStateRunnerPath(root, path) {
		return managedRunnerExecutable(root, path, "")
	}
	if filepath.Base(path) == stateRunnerName {
		digest, err := protectedStateExecutableDigest(path)
		return err == nil && digest == binding.WorkerSHA256
	}
	return CurrentManagedRunnerExecutable(root, path) && strings.TrimPrefix(filepath.Base(path), stateRunnerName+"-") == binding.WorkerSHA256
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
	if meta == nil || meta.RunnerOwnership != "" || td == nil {
		return PersistedRunnerOwnership(root, meta), nil
	}
	migrated, err := td.HasManagedRunnerMigration()
	if err != nil {
		return "", err
	}
	if migrated {
		return task.RunnerOwnershipManaged, nil
	}
	return PersistedRunnerOwnership(root, meta), nil
}

func CurrentManagedRunnerExecutable(root, path string) bool {
	if path == "" {
		return false
	}
	if IsStateRunnerPath(root, filepath.Clean(path)) {
		name := filepath.Base(path)
		digest := strings.TrimPrefix(name, stateRunnerName+"-")
		if digest == name {
			return false
		}
		actualDigest, err := protectedStateExecutableDigest(path)
		return err == nil && actualDigest == digest
	}
	info, err := buildinfo.ReadFile(path)
	return err == nil && ManagedRunnerBuildInfo(info)
}

func LegacyInspectionRetryRunner(store *taskdir.Store, td *taskdir.TaskDir, meta *task.MetaRecord) (string, bool, error) {
	if meta == nil || meta.RunnerOwnership != "" {
		return "", false, nil
	}
	if store == nil || td == nil {
		return "", false, task.ErrInvalidPermit
	}
	inspected, err := taskHasInspectionOperation(store, meta.TaskID)
	if err != nil || !inspected {
		return "", false, err
	}
	if meta.RunnerExecutable == "" {
		return "", false, task.ErrEvidenceFault
	}
	return meta.RunnerExecutable, true, nil
}

func taskHasInspectionOperation(store *taskdir.Store, taskID string) (bool, error) {
	if store == nil {
		return false, task.ErrInvalidPermit
	}
	operation, err := inspection.LoadOperation(store, taskID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, operation.Close()
}
