package claude

import (
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// RuntimeRequirements returns the flags used by the native Claude launch
// plan. It returns a fresh slice so package metadata remains immutable.
func RuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{
		RequiredFlags: []string{
			"--print", "--input-format", "--output-format", "--verbose", "--permission-mode",
			"--permission-prompts", "--resume", "--session-id",
		},
	}
}

// legacyRuntimeRequirements preserves the exact capability probe used by
// historical Claude profiles. It is intentionally separate from the native
// admission requirements above.
func legacyRuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{
		RequiredFlags: []string{
			"--print", "--input-format", "--output-format", "--verbose", "--safe-mode", "--restricted",
			"--tools", "--disallowedTools", "--strict-mcp-config", "--mcp-config", "--settings", "--permission-mode",
			"--permission-prompts", "--disable-slash-commands", "--no-chrome", "--resume", "--session-id",
		},
	}
}

func resolveExecutable() (commonprovider.CLIInfo, error) {
	info, err := commonprovider.LocateCLI("claude")
	if err != nil {
		return commonprovider.CLIInfo{}, fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return info, nil
}
