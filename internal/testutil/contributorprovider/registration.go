package contributorprovider

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// RuntimeRequirements returns the flags used by the synthetic fixture's
// command. It is surfaced only by the test catalog and is still checked by
// the same runtime preflight as native adapters.
func RuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{RequiredFlags: []string{
		"--runtime-config", "--tools-config", "--output", "--task-id", "--session-id", "--resume",
	}}
}

// Description returns the finite fixture's static capabilities. The fixture
// executable is inspected when a task is prepared.
func Description() commonprovider.Description {
	return commonprovider.Description{
		ID:               Provider,
		SupportedModes:   []string{Mode},
		SupportedOptions: []string{commonprovider.OptionContinuation},
		Runtime:          RuntimeRequirements(),
		Discoverable:     true,
	}
}

func Registration() commonprovider.Registration {
	return commonprovider.Registration{
		Description:  Description(),
		Prepare:      PrepareCandidate,
		Interpreters: []predicate.Interpreter{NewInterpreter()},
	}
}
