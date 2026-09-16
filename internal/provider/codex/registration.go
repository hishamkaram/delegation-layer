package codex

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// Description returns Codex's static request capabilities. The executable
// and its supported flags are checked by the shared supervised inspection
// before a task is finalized for launch.
func Description() commonprovider.Description {
	return commonprovider.Description{
		ID:               Provider,
		SupportedModes:   []string{Mode, WorkspaceWriteMode},
		SupportedOptions: []string{commonprovider.OptionContinuation},
		Runtime:          RuntimeRequirements(),
		Discoverable:     true,
	}
}

// Registration returns Codex's explicit catalog entry. The interpreter
// describes the output contract; it does not qualify a particular CLI build.
func Registration() commonprovider.Registration {
	return commonprovider.Registration{
		Description:  Description(),
		Prepare:      PrepareCandidate,
		Interpreters: []predicate.Interpreter{NewInterpreter(Mode), NewInterpreter(WorkspaceWriteMode), newLegacyInterpreter()},
	}
}
