package antigravity

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// Description returns the static capabilities of the agy adapter. Runtime
// availability and flag compatibility are checked when a task is prepared.
func Description() commonprovider.Description {
	return commonprovider.Description{
		ID:               Provider,
		SupportedModes:   []string{Mode},
		SupportedOptions: []string{commonprovider.OptionContinuation, commonprovider.OptionNativeTimeout},
		Continuation:     commonprovider.ContinuationNative,
		Runtime:          RuntimeRequirements(),
		Discoverable:     true,
	}
}

// Registration returns agy's explicit catalog entry. Both predicate
// revisions remain available for historical collection; fresh preparation
// selects the current interpreter and requests a supervised runtime probe.
func Registration() commonprovider.Registration {
	return commonprovider.Registration{
		Description:     Description(),
		Prepare:         PrepareCandidate,
		PrepareExisting: PrepareExistingCandidate,
		Interpreters:    []predicate.Interpreter{NewPrintInterpreter(), NewCurrentPrintInterpreter()},
	}
}
