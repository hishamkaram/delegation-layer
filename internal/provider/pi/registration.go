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
		SupportedModes: []string{ModeReadOnly},
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

// Registration returns Pi's read-only interpreter. Pi's built-in write and
// edit tools do not provide a native workspace boundary, so workspace-write
// is intentionally unavailable until the CLI exposes one.
func Registration() commonprovider.Registration {
	return commonprovider.Registration{
		Description: Description(),
		Prepare:     PrepareCandidate,
		Interpreters: []predicate.Interpreter{
			NewInterpreter(ModeReadOnly),
		},
	}
}
