package tasklifecycle

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func BuildContinuationRequest(a DispatchRequest, root string, predecessor *task.TaskRecord, meta *task.MetaRecord, brief []byte) (DispatchRequest, error) {
	continuationArgs := a
	continuationArgs.Root = root
	continuationArgs.Auto = false
	continuationArgs.TaskID = ""
	continuationArgs.ResumeTask = predecessor.TaskID
	continuationArgs.Provider = predecessor.Provider
	continuationArgs.BriefPath = ""
	continuationArgs.BriefBytes = brief
	continuationArgs.Cwd = predecessor.CanonicalCwd
	if predecessor.Workspace != nil {
		binding := *predecessor.Workspace
		continuationArgs.WorkspaceID = binding.ID
		continuationArgs.WorkspaceBinding = &binding
	}
	continuationArgs.Config = ContinuationConfig(predecessor.RequestedConfig, a.Config)
	if meta != nil {
		saved := meta.SupervisorConfig
		continuationArgs.Selection.SavedSupervisor = &saved
	}
	if meta != nil {
		continuationArgs.Selection.RunnerOwnership = supervisorcore.PersistedRunnerOwnership(root, meta)
		if continuationArgs.Selection.RunnerOwnership == task.RunnerOwnershipManaged {
			continuationArgs.Selection.Runner = ""
		} else if continuationArgs.Selection.Runner == "" {
			runner, err := supervisorcore.ContinuationRunner(root, meta.RunnerExecutable, continuationArgs.Selection.RunnerOwnership)
			if err != nil {
				return DispatchRequest{}, err
			}
			continuationArgs.Selection.Runner = runner
		}
	}
	if continuationArgs.Selection.ConfigPath == "" && meta != nil && !pueue.IsPrivateConfig(root, meta.SupervisorConfig.ConfigPath) {
		continuationArgs.Selection.ConfigPath = meta.SupervisorConfig.ConfigPath
	}
	return continuationArgs, nil
}

func ValidateContinuationMetadata(root string, selection supervisorcore.AdmissionSelection, meta *task.MetaRecord) error {
	if err := ValidateContinuationEnvironment(meta); err != nil {
		return err
	}
	return supervisorcore.ValidateContinuationSelection(root, selection, meta)
}
