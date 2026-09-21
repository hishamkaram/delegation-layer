package opencode

import (
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// RuntimeRequirements describes only the command and flags used by the
// adapter. The shared supervised inspection accepts any valid reported CLI
// version when these capabilities are present.
func RuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{
		HelpArgs: []string{"run"},
		RequiredFlags: []string{
			"--format", "--dir", "--session", "--model", "--variant", "--agent", "--auto",
		},
	}
}

// legacyRuntimeRequirements retains the historical capability description for
// reconstruction of tasks admitted with the adapter-owned pure-mode profile.
func legacyRuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{
		HelpArgs:      []string{"run"},
		RequiredFlags: []string{"--format", "--dir", "--session", "--model", "--variant", "--agent", "--pure", "--auto"},
	}
}

func resolveExecutable() (commonprovider.CLIInfo, error) {
	info, err := commonprovider.LocateCLI("opencode")
	if err != nil {
		return commonprovider.CLIInfo{}, fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return info, nil
}
