package admission

import (
	"slices"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// RuntimeCapabilityFor derives observations without claiming live acceptance.
func RuntimeCapabilityFor(candidate commonprovider.ProfileCandidate, profile commonprovider.PreparedProfile) *RuntimeCapabilityEvidence {
	if candidate.Inspection == nil || candidate.Inspection.Runtime == nil || profile.RuntimePreflightSHA256 != "" {
		return nil
	}
	runtime := candidate.Inspection.Runtime
	return &RuntimeCapabilityEvidence{Version: profile.ObservedVersion, ExecutableSHA256: runtime.ExecutableSHA256, HelpArgs: slices.Clone(runtime.HelpArgs), RequiredFlags: slices.Clone(runtime.RequiredFlags)}
}
