package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ResolveInitialConfig selects the explicit configuration, else the launch
// scope's DELEGATE_PUEUE_CONFIG value. It never reads the process environment.
func ResolveInitialConfig(selection AdmissionSelection) (string, error) {
	path := selection.ConfigPath
	if path == "" {
		path = selection.EnvironmentConfigPath
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
	return installation.ResolveRunner(argument, runtime.RunnerExecutable)
}

func AdmissionQueueRepairRunner(selection AdmissionSelection, runtime AdmissionOptions, taskRunner string) (string, error) {
	if RequestedRunnerOwnership(selection) == task.RunnerOwnershipManaged {
		return taskRunner, nil
	}
	runner, err := installation.ResolveRunner("", runtime.RunnerExecutable)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return runner, nil
}

func AdmissionSupervisorExecutablePair(selection AdmissionSelection, root string, runtime AdmissionOptions) (string, string, error) {
	if selection.ContinuationSource != (infrastructure.SavedBindingSource{}) {
		client, daemon, found, err := publishedContinuationSupervisorPair(root, runtime.Lookup())
		if err != nil || found {
			return client, daemon, err
		}
	}
	if selection.SavedSupervisor != nil {
		return ResolveRecoveryPrivateSupervisorExecutables(root, *selection.SavedSupervisor, runtime.Lookup())
	}
	return installation.ResolvePrivateSupervisorExecutables(runtime.Lookup())
}

// The published generation owns the recovery candidate.
// Its recorded installation paths remain preferred, and missing
// paths use canonical discovery. Selection grants no native launch authority.
func publishedContinuationSupervisorPair(root string, lookup installation.ExecutableLookup) (string, string, bool, error) {
	published, found, err := pueue.ReadPrivateSupervisorBinding(root)
	if err != nil || !found {
		return "", "", found, err
	}
	client, daemon, err := ResolveRecoveryPrivateSupervisorExecutables(root, published, lookup)
	return client, daemon, true, err
}
