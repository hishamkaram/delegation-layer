package claude

import (
	"fmt"
	"slices"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// RuntimeRequirements returns the flags used by the native Claude launch
// plan. It returns a fresh slice so package metadata remains immutable.
func RuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{
		// Claude's non-TTY help output is intentionally truncated by recent
		// releases. Probe the stable print entry point here; the complete
		// launch argv remains validated by the provider process itself.
		RequiredFlags: []string{"--print"},
	}
}

func runtimeRequirementsForRequest(request task.TaskRecord) commonprovider.RuntimeCapability {
	return RuntimeRequirements()
}

// runtimeRequirementsForPredicate keeps the admitted probe attached to its
// native predicate. Only references created with the relaxed probe use the
// current stable flag set; older references retain their original check.
func runtimeRequirementsForPredicate(request task.TaskRecord, reference task.PredicateRef) commonprovider.RuntimeCapability {
	if reference.Equal(ReferenceForMode(request.Mode)) || reference.Equal(interactionReference(request.Mode)) {
		return runtimeRequirementsForRequest(request)
	}
	return historicalNativeRuntimeRequirementsForRequest(request)
}

func historicalNativeRuntimeRequirementsForRequest(request task.TaskRecord) commonprovider.RuntimeCapability {
	requirements := commonprovider.RuntimeCapability{
		RequiredFlags: []string{
			"--print", "--input-format", "--output-format", "--verbose", "--permission-mode",
			"--permission-prompts", "--resume", "--session-id",
		},
	}
	return appendModelRuntimeFlags(requirements, request)
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

func legacyRuntimeRequirementsForRequest(request task.TaskRecord) commonprovider.RuntimeCapability {
	return appendModelRuntimeFlags(legacyRuntimeRequirements(), request)
}

func appendModelRuntimeFlags(requirements commonprovider.RuntimeCapability, request task.TaskRecord) commonprovider.RuntimeCapability {
	requirements.RequiredFlags = slices.Clone(requirements.RequiredFlags)
	if request.RequestedConfig.Model != "" {
		requirements.RequiredFlags = append(requirements.RequiredFlags, "--model")
	}
	if effort := request.RequestedConfig.Effort; effort != "" && effort != "default" {
		requirements.RequiredFlags = append(requirements.RequiredFlags, "--effort")
	}
	return requirements
}

func resolveExecutable() (commonprovider.CLIInfo, error) {
	info, err := commonprovider.LocateCLI("claude")
	if err != nil {
		return commonprovider.CLIInfo{}, fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return info, nil
}
