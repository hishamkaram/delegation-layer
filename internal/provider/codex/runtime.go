package codex

import (
	"fmt"
	"slices"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// RuntimeRequirements returns the command and flags used by the Codex launch
// plan. It returns fresh slices so package metadata remains immutable.
func RuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{
		HelpArgs: []string{"exec"},
		RequiredFlags: []string{
			"-c", "--sandbox", "--cd",
			"--output-last-message", "--json", "--color",
		},
	}
}

func runtimeRequirementsForRequest(request task.TaskRecord, requirements commonprovider.RuntimeCapability) commonprovider.RuntimeCapability {
	requirements.RequiredFlags = slices.Clone(requirements.RequiredFlags)
	if request.RequestedConfig.Model != "" {
		requirements.RequiredFlags = append(requirements.RequiredFlags, "--model")
	}
	return requirements
}

func legacyRuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{
		HelpArgs: []string{"exec"},
		RequiredFlags: []string{
			"-c", "--strict-config", "--sandbox", "--cd", "--ignore-user-config", "--ignore-rules",
			"--output-last-message", "--json", "--color",
		},
	}
}

func resolveExecutable() (commonprovider.CLIInfo, error) {
	// Preparation only discovers and fingerprints the executable. The version,
	// help output, and required flags are checked by the shared supervised
	// runtime inspection after the candidate has been admitted.
	info, err := commonprovider.LocateCLI("codex")
	if err != nil {
		return commonprovider.CLIInfo{}, fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return info, nil
}
