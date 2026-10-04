// Package ccr prepares foreground Claude Code Router turns for the shared supervisor.
package ccr

import (
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const Provider = "ccr:print"

// Reference binds CCR foreground completion to the shared native Claude stream
// contract, including CCR's own exit status and adapter/session identity.
func Reference(mode string) task.PredicateRef {
	contract := "ccr-foreground-v1\nprovider-only; explicit alias; foreground exit must succeed; session provider is ccr:print\n" + claude.ContractForMode(mode)
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "ccr-foreground-v1", SHA256: task.ComputeSHA256([]byte(contract))}
}

func RuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{HelpArgs: []string{"launch"}, RequiredFlags: []string{"--model", "--auth-mode", "--print", "--permission-mode"}}
}

func Registration() commonprovider.Registration {
	interpreters := make([]predicate.Interpreter, 0, 2)
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		interpreter, err := claude.NewStreamInterpreter(Reference(mode))
		if err != nil {
			panic(err)
		} // Immutable compiled contract.
		interpreters = append(interpreters, interpreter)
	}
	return commonprovider.Registration{
		Description: commonprovider.Description{
			ID: Provider, Discoverable: true,
			SupportedModes:   []string{config.ModeReadOnly, config.ModeWorkspaceWrite},
			SupportedOptions: []string{commonprovider.OptionModel, commonprovider.OptionEffort, commonprovider.OptionContinuation},
			RequiredOptions:  []string{commonprovider.OptionModel},
			Continuation:     commonprovider.ContinuationNative,
			ReadOnly:         commonprovider.ReadOnlyCapability{Supported: true, Mechanism: commonprovider.ReadOnlyNativePlan, Containment: commonprovider.ReadOnlyProviderOwned},
			Runtime:          RuntimeRequirements(),
		},
		Prepare: PrepareCandidate, PrepareExisting: PrepareExistingCandidate,
		Models: ModelDiscovery, Interpreters: interpreters,
	}
}
