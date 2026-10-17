package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ResolveInitialConfig(selection AdmissionSelection) (string, error) {
	path := selection.ConfigPath
	if path == "" {
		path = os.Getenv("DELEGATE_PUEUE_CONFIG")
	}
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", pueue.ErrConfiguration
	}
	return path, nil
}

func AdmissionUsesPrivateSupervisor(selection AdmissionSelection, root string) (bool, error) {
	if selection.SavedSupervisor != nil {
		return pueue.IsPrivateConfig(root, selection.SavedSupervisor.ConfigPath), nil
	}
	configPath, err := ResolveInitialConfig(selection)
	if err != nil {
		return false, err
	}
	if configPath != "" && pueue.IsPrivateConfig(root, configPath) {
		return false, fmt.Errorf("%w: explicit supervisor config collides with the private state-rooted supervisor", pueue.ErrConfiguration)
	}
	return configPath == "", nil
}

func ResolveAdmissionRunner(argument, ownership string, runtime AdmissionOptions) (string, error) {
	if ownership == task.RunnerOwnershipManaged {
		argument = ""
	} else if argument == "" {
		return "", task.ErrEvidenceFault
	}
	return ResolveRunner(argument, runtime.RunnerExecutable)
}

func AdmissionQueueRepairRunner(selection AdmissionSelection, runtime AdmissionOptions, taskRunner string) (string, error) {
	if RequestedRunnerOwnership(selection) == task.RunnerOwnershipManaged {
		return taskRunner, nil
	}
	runner, err := ResolveRunner("", runtime.RunnerExecutable)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return runner, nil
}

func AdmissionSupervisorExecutablePair(selection AdmissionSelection, root string, runtime AdmissionOptions) (string, string, error) {
	if selection.SavedSupervisor != nil && PrivateSupervisorExecutablesAvailable(selection.SavedSupervisor.ClientExecutable, selection.SavedSupervisor.DaemonExecutable) {
		return selection.SavedSupervisor.ClientExecutable, selection.SavedSupervisor.DaemonExecutable, nil
	}
	if selection.SavedSupervisor != nil {
		return ResolvePrivateSupervisorExecutables(root, runtime.InitialExecutable)
	}
	// Fresh admission stages the active install as successor inventory. The
	// completed publication remains the ordinary active-generation authority.
	return ResolveCurrentPrivateSupervisorExecutables(root, runtime.InitialExecutable)
}
