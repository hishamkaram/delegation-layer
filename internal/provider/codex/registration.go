package codex

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Description returns Codex's static request capabilities. The executable
// and its supported flags are checked by the shared supervised inspection
// before a task is finalized for launch.
func Description() commonprovider.Description {
	return commonprovider.Description{
		ID:               Provider,
		Interaction:      task.InteractionCapabilities{SupportedModes: []string{Mode, WorkspaceWriteMode}, Clarification: true, Steering: true},
		SupportedModes:   []string{Mode, WorkspaceWriteMode},
		SupportedOptions: []string{commonprovider.OptionInteractive, commonprovider.OptionContinuation, commonprovider.OptionEffort, commonprovider.OptionModel},
		Continuation:     commonprovider.ContinuationNative,
		ReadOnly:         commonprovider.ReadOnlyCapability{Supported: true, Mechanism: commonprovider.ReadOnlyNativeSandbox, Containment: commonprovider.ReadOnlyProviderOwned},
		Runtime:          RuntimeRequirements(),
		Discoverable:     true,
	}
}

// Registration returns Codex's explicit catalog entry. The interpreter
// describes the output contract; it does not qualify a particular CLI build.
func Registration() commonprovider.Registration {
	return commonprovider.Registration{
		Description:     Description(),
		Prepare:         PrepareCandidate,
		PrepareExisting: PrepareExistingCandidate,
		Interpreters: []predicate.Interpreter{
			NewInterpreter(Mode), NewInterpreter(WorkspaceWriteMode),
			newHistoricalV3Interpreter(Mode), newHistoricalV3Interpreter(WorkspaceWriteMode),
			newHistoricalCurrentInterpreter(Mode), newHistoricalCurrentInterpreter(WorkspaceWriteMode),
			newHistoricalRuntimeInterpreter(Mode), newHistoricalRuntimeInterpreter(WorkspaceWriteMode),
			newLegacyInterpreter(),
			newInteractionInterpreter(Mode), newInteractionInterpreter(WorkspaceWriteMode),
			newHistoricalInteractionInterpreter(Mode), newHistoricalInteractionInterpreter(WorkspaceWriteMode),
		},
	}
}

// ReviewDescription returns Codex's explicit read-only review capability.
func ReviewDescription() commonprovider.Description {
	return commonprovider.Description{
		ID:               ReviewProvider,
		SupportedModes:   []string{Mode},
		SupportedOptions: []string{commonprovider.OptionEffort, commonprovider.OptionModel, commonprovider.OptionReviewTarget},
		RequiredOptions:  []string{commonprovider.OptionReviewTarget},
		Continuation:     commonprovider.ContinuationUnsupported,
		ReadOnly:         commonprovider.ReadOnlyCapability{Supported: true, Mechanism: commonprovider.ReadOnlyNativeSandbox, Containment: commonprovider.ReadOnlyProviderOwned},
		Runtime:          reviewRuntimeRequirementsForRequest(task.TaskRecord{}),
		Discoverable:     true,
	}
}

// ReviewRegistration returns the bounded Codex review operation. It shares
// executable discovery and the supervisor lifecycle with codex:exec while
// retaining a distinct request and output predicate identity.
func ReviewRegistration() commonprovider.Registration {
	return commonprovider.Registration{
		Description:     ReviewDescription(),
		Prepare:         PrepareReviewCandidate,
		PrepareExisting: PrepareExistingReviewCandidate,
		Interpreters:    []predicate.Interpreter{newReviewInterpreter()},
	}
}
