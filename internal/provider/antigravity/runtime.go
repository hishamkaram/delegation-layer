package antigravity

import (
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

type runtimeIdentity struct {
	Executable string `json:"executable"`
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
}

// RuntimeRequirements returns the flags used by the agy launch plan. A fresh
// value keeps package metadata immutable while allowing catalog callers to
// retain the returned slices safely.
func RuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{
		RequiredFlags: []string{
			"--input-format", "--output-format", "--sandbox", "--mode", "--add-dir",
			"--print-timeout", "--conversation",
		},
	}
}

func decodeRuntimeIdentity(facts commonprovider.InspectionFacts, located commonprovider.CLIInfo) (runtimeIdentity, error) {
	if facts.Runtime == nil {
		return runtimeIdentity{}, fmt.Errorf("%w: supervised runtime capability facts are unavailable", ErrUnsupportedProfile)
	}
	observed := *facts.Runtime
	if observed.Executable != located.Path || observed.SHA256 != located.SHA256 {
		return runtimeIdentity{}, fmt.Errorf("%w: runtime executable changed between discovery and launch", ErrUnsupportedProfile)
	}
	return runtimeIdentity{Executable: observed.Executable, Version: observed.Version, SHA256: observed.SHA256}, nil
}

func hashRuntimeExecutable(path string) (string, error) {
	return commonprovider.FingerprintExecutable(path)
}
