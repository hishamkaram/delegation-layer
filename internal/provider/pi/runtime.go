package pi

import (
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// RuntimeRequirements describes the command-level capabilities used by one Pi
// launch mode. It is intentionally version-independent: the shared supervised
// probe accepts any CLI release that advertises the flags used by that mode.
// The omitted mode preserves the read-only catalog description.
func RuntimeRequirements(mode ...string) commonprovider.RuntimeCapability {
	selected := ModeReadOnly
	if len(mode) > 0 && mode[0] != "" {
		selected = mode[0]
	}
	if selected == ModeWorkspaceWrite {
		return commonprovider.RuntimeCapability{RequiredFlags: []string{
			"--mode", "--model", "--thinking", "--session",
		}}
	}
	return commonprovider.RuntimeCapability{
		HelpArgs: []string{"--no-extensions"},
		RequiredFlags: []string{
			"--mode", "--tools", "--model", "--thinking", "--session", "--session-dir", "--no-extensions",
		},
	}
}

func historicalNativeRuntimeRequirements() commonprovider.RuntimeCapability {
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
