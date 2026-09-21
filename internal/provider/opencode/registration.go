package opencode

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// Description returns static OpenCode request capabilities. The executable
// and its command flags are checked by the shared supervised runtime probe.
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

// Registration returns OpenCode's explicit catalog entry. Each permission
// mode has a distinct predicate reference because mode is part of the
// immutable output contract.
func Registration() commonprovider.Registration {
	return commonprovider.Registration{
		Description:     Description(),
		Prepare:         PrepareCandidate,
		PrepareExisting: PrepareExistingCandidate,
		Interpreters: []predicate.Interpreter{
			NewInterpreter(ModeReadOnly),
			NewInterpreter(ModeWorkspaceWrite),
		},
	}
}
