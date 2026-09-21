package pi

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// Description returns Pi's static request capabilities. Executable and flag
// compatibility are checked by the shared supervised runtime probe.
func Description() commonprovider.Description {
	return commonprovider.Description{
		ID:             Provider,
		SupportedModes: []string{ModeReadOnly, ModeWorkspaceWrite},
		SupportedOptions: []string{
			commonprovider.OptionContinuation,
			commonprovider.OptionEffort,
			commonprovider.OptionModel,
		},
		Continuation: commonprovider.ContinuationNative,
		Runtime:      RuntimeRequirements(),
		Discoverable: true,
	}
}

// Registration returns Pi's native interpreters. The workspace-write
// interpreter preserves Pi's configured tools and permission behavior.
func Registration() commonprovider.Registration {
	return commonprovider.Registration{
		Description:     Description(),
		Prepare:         PrepareCandidate,
		PrepareExisting: PrepareExistingCandidate,
		Interpreters: []predicate.Interpreter{
			NewInterpreter(ModeReadOnly),
			NewInterpreter(ModeWorkspaceWrite),
			newReadOnlyV2Interpreter(),
			newLegacyInterpreter(),
		},
	}
}
