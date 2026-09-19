package claude

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// Description returns Claude's static request capabilities. Runtime
// availability and flag compatibility are checked when a task is prepared.
func Description() commonprovider.Description {
	return commonprovider.Description{
		ID:               Provider,
		SupportedModes:   []string{Mode, WorkspaceWriteMode},
		SupportedOptions: []string{commonprovider.OptionContinuation},
		Runtime:          RuntimeRequirements(),
		Discoverable:     true,
	}
}

// Registration returns Claude's explicit catalog entry. The interpreter
// describes the stream contract; it does not qualify a particular CLI build.
func Registration() commonprovider.Registration {
	return commonprovider.Registration{
		Description:     Description(),
		Prepare:         PrepareCandidate,
		PrepareExisting: PrepareExistingCandidate,
		Interpreters:    []predicate.Interpreter{NewInterpreter(Mode), NewInterpreter(WorkspaceWriteMode), newLegacyInterpreter()},
	}
}
