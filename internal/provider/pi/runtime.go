package pi

import (
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// RuntimeRequirements describes the command-level capabilities used by both
// Pi permission profiles. It is intentionally version-independent: the shared
// supervised probe accepts any CLI release that advertises these flags.
func RuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{
		// The capability probe itself is a Pi process. Keep it from loading
		// extensions or attempting package/network startup work before it prints
		// help; the launch profile applies the same controls to read-only runs.
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
