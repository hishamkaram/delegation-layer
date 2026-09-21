package pi

import (
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// RuntimeRequirements describes the command-level capabilities used by the Pi
// read-only profile. It is intentionally version-independent: the shared
// supervised probe accepts any CLI release that advertises these flags.
func RuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{
		HelpArgs: []string{},
		RequiredFlags: []string{
			"--mode", "--tools", "--model", "--thinking", "--session",
		},
	}
}

func legacyRuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{
		HelpArgs: []string{"--no-extensions", "--offline"},
		RequiredFlags: []string{
			"--mode", "--tools", "--model", "--thinking", "--session", "--session-dir", "--no-extensions", "--offline",
		},
	}
}

func resolveExecutable() (commonprovider.CLIInfo, error) {
	info, err := commonprovider.LocateCLI("pi")
	if err != nil {
		return commonprovider.CLIInfo{}, fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return info, nil
}
