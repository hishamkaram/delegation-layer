package pi

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
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
		ReadOnly:     commonprovider.ReadOnlyCapability{Supported: true, Mechanism: commonprovider.ReadOnlyToolAllowlist, Containment: commonprovider.ReadOnlyProviderOwned},
		Runtime:      RuntimeRequirements(),
		Discoverable: true,
	}
}

// Registration returns Pi's native interpreters. The workspace-write
// interpreter selects the documented built-in write tools while Pi owns its
// permission behavior and extensions.
func Registration() commonprovider.Registration {
	return commonprovider.Registration{
		Description:          Description(),
		Prepare:              PrepareCandidate,
		PrepareExisting:      PrepareExistingCandidate,
		ValidateContinuation: validateContinuation,
		Interpreters: []predicate.Interpreter{
			NewInterpreter(ModeReadOnly),
			NewInterpreter(ModeWorkspaceWrite),
			newHistoricalV7Interpreter(ModeWorkspaceWrite),
			newHistoricalV5Interpreter(ModeReadOnly),
			newHistoricalV5Interpreter(ModeWorkspaceWrite),
			newHistoricalV4Interpreter(ModeReadOnly),
			newHistoricalV4Interpreter(ModeWorkspaceWrite),
			newHistoricalV3Interpreter(ModeReadOnly),
			newHistoricalV3Interpreter(ModeWorkspaceWrite),
			newReadOnlyV2Interpreter(),
			newLegacyInterpreter(),
		},
	}
}

func validateContinuation(predecessor task.TaskRecord, _ task.MetaRecord, successor task.TaskRecord) error {
	if predecessor.CanonicalCwd != successor.CanonicalCwd {
		// Pi's exact-session resume searches globally and prompts to fork when
		// the stored session belongs to another workspace. The JSON adapter
		// cannot answer that prompt while preserving session identity.
		return task.ErrIdentityMismatch
	}
	return nil
}
