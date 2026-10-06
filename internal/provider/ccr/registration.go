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

const (
	currentPredicateVersion    = "ccr-foreground-v2"
	historicalPredicateVersion = "ccr-foreground-v1"
)

// Reference binds fresh CCR foreground completion to the corrected native
// Claude stream contract, including CCR's own exit status and adapter/session
// identity.
func Reference(mode string) task.PredicateRef {
	contract := ccrContract(currentPredicateVersion, claude.ContractForMode(mode))
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: currentPredicateVersion, SHA256: task.ComputeSHA256([]byte(contract))}
}

// HistoricalReference identifies CCR evidence sealed under the pre-issue47
// Claude parser. It remains registered solely so sealed tasks can replay.
func HistoricalReference(mode string) task.PredicateRef {
	contract := ccrContract(historicalPredicateVersion, claude.HistoricalNativeContractForMode(mode))
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: historicalPredicateVersion, SHA256: task.ComputeSHA256([]byte(contract))}
}

func ccrContract(version, claudeContract string) string {
	if claudeContract == "" {
		return ""
	}
	return version + "\nprovider-only; explicit alias; foreground exit must succeed; session provider is ccr:print\n" + claudeContract
}

func RuntimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{VersionArgs: []string{"version"}, HelpArgs: []string{"launch"}, RequiredFlags: []string{"--model", "--auth-mode", "--print", "--permission-mode"}}
}

func Registration() commonprovider.Registration {
	interpreters := make([]predicate.Interpreter, 0, 4)
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		interpreter, err := claude.NewStreamInterpreterWithRecovery(Reference(mode), HistoricalReference(mode))
		if err != nil {
			panic(err)
		} // Immutable compiled contract.
		interpreters = append(interpreters, interpreter)
	}
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		interpreter, err := claude.NewHistoricalStreamInterpreter(HistoricalReference(mode))
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
