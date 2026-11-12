package claude

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Description returns Claude's static request capabilities. Runtime
// availability and flag compatibility are checked when a task is prepared.
func Description() commonprovider.Description {
	return commonprovider.Description{
		ID:               Provider,
		Interaction:      task.InteractionCapabilities{SupportedModes: []string{Mode, WorkspaceWriteMode}, Clarification: true},
		SupportedModes:   []string{Mode, WorkspaceWriteMode},
		SupportedOptions: []string{commonprovider.OptionInteractive, commonprovider.OptionContinuation, commonprovider.OptionEffort, commonprovider.OptionModel},
		Continuation:     commonprovider.ContinuationNative,
		ReadOnly:         commonprovider.ReadOnlyCapability{Supported: true, Mechanism: commonprovider.ReadOnlyNativePlan, Containment: commonprovider.ReadOnlyProviderOwned},
		Runtime:          RuntimeRequirements(),
		Discoverable:     true,
	}
}

// Registration returns Claude's explicit catalog entry. The interpreter
// describes the stream contract; it does not qualify a particular CLI build.
func Registration() commonprovider.Registration {
	return commonprovider.Registration{
		Description:      Description(),
		Prepare:          PrepareCandidate,
		PrepareExisting:  PrepareExistingCandidate,
		PrepareSuccessor: PrepareSuccessorCandidate,
		Interpreters: []predicate.Interpreter{
			NewInterpreter(Mode), NewInterpreter(WorkspaceWriteMode),
			newFollowUpInterpreter(Mode), newFollowUpInterpreter(WorkspaceWriteMode),
			newFollowUpInteractionInterpreter(Mode), newFollowUpInteractionInterpreter(WorkspaceWriteMode),
			newHistoricalNativeInterpreter(Mode),
			newHistoricalNativeInterpreter(WorkspaceWriteMode),
			newLegacyNativeInterpreter(),
			newLegacyNativeWorkspaceWriteInterpreter(),
			newLegacyPortableInterpreter(Mode), newLegacyPortableInterpreter(WorkspaceWriteMode),
			newLegacyInterpreter(),
			newInteractionInterpreter(Mode),
			newInteractionInterpreter(WorkspaceWriteMode),
			interactionInterpreter{mode: Mode, historical: true},
			interactionInterpreter{mode: WorkspaceWriteMode, historical: true},
			newPreviousNativeProbeInterpreter(Mode),
			newPreviousNativeProbeInterpreter(WorkspaceWriteMode),
			newPreviousInteractionInterpreter(Mode),
			newPreviousInteractionInterpreter(WorkspaceWriteMode),
			newPreAttributionInterpreter(Mode),
			newPreAttributionInterpreter(WorkspaceWriteMode),
			newPreAttributionInteractionInterpreter(Mode),
			newPreAttributionInteractionInterpreter(WorkspaceWriteMode),
			newUnscopedInterpreter(WorkspaceWriteMode),
			newUnscopedInteractionInterpreter(Mode),
			newUnscopedInteractionInterpreter(WorkspaceWriteMode),
			newUnscopedInterpreter(Mode),
			newScopedInterpreter(Mode),
			newScopedInterpreter(WorkspaceWriteMode),
			newScopedInteractionInterpreter(Mode),
			newScopedInteractionInterpreter(WorkspaceWriteMode),
			newAdvisoryInterpreter(Mode), newAdvisoryInterpreter(WorkspaceWriteMode),
			newAdvisoryInteractionInterpreter(Mode), newAdvisoryInteractionInterpreter(WorkspaceWriteMode),
			newProjectionInterpreter(Mode), newProjectionInterpreter(WorkspaceWriteMode),
		},
	}
}
