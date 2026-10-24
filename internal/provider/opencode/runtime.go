package opencode

import (
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// RuntimeRequirements describes the current ACP launch. `opencode acp` takes
// no flag from the adapter, so the shared supervised inspection checks only
// that its help command succeeds. Help alone proves no protocol behavior: the
// adapter's protocol validates the native handshake, configuration and
// terminal response on every run. Any valid reported CLI version is accepted.
func RuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{HelpArgs: []string{"acp"}}
}

// nativeV2RuntimeRequirements retains the capability description bound to
// tasks admitted with the historical native run dialect.
func nativeV2RuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{
		HelpArgs:      []string{"run"},
		RequiredFlags: []string{"--format", "--session", "--model", "--agent", "--auto", "--standalone"},
	}
}

// nativeV1RuntimeRequirements retains the capability description bound to
// tasks admitted with the historical native dialect.
func nativeV1RuntimeRequirements() commonprovider.RuntimeCapability {
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

func resolveExecutable(environment []string) (commonprovider.CLIInfo, error) {
	info, err := commonprovider.LocateCLI("opencode", environment)
	if err != nil {
		return commonprovider.CLIInfo{}, fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return info, nil
}
