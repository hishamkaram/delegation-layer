// Package ccr prepares foreground Claude Code Router turns for the shared supervisor.
package ccr

import (
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const Provider = "ccr:print"

const (
	currentPredicateVersion    = "ccr-foreground-v3"
	modeledPredicateVersion    = "ccr-foreground-v3-modeled"
	previousPredicateVersion   = "ccr-foreground-v2"
	historicalPredicateVersion = "ccr-foreground-v1"
)

type preparationRevision uint8

const (
	legacyPreparation preparationRevision = iota + 1
	resumeRoutePreparation
)

type inspectionRevision uint8

const (
	legacyInspection inspectionRevision = iota + 1
	routeInspection
)

type preparationContract struct {
	launch     preparationRevision
	inspection inspectionRevision
}

// Reference binds fresh CCR foreground completion to the corrected native
// Claude stream contract, including CCR's own exit status and adapter/session
// identity.
func Reference(mode string) task.PredicateRef {
	contract := ccrContract(currentPredicateVersion, claude.ContractForMode(mode), true)
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: currentPredicateVersion, SHA256: task.ComputeSHA256([]byte(contract))}
}

// modeledReference identifies tasks admitted by the route-capability probe
// when the installed CCR does not expose --resume-route. Their launch remains
// modeled explicitly, while reconstruction must replay the route probe that
// selected that fallback.
func modeledReference(mode string) task.PredicateRef {
	contract := ccrContract(modeledPredicateVersion, claude.ContractForMode(mode), false)
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: modeledPredicateVersion, SHA256: task.ComputeSHA256([]byte(contract))}
}

// previousReference identifies tasks admitted before CCR gained its dedicated
// route-preserving continuation flag. Their launch contract remains intact on
// reconstruction, including the original model and provider-only auth flags.
func previousReference(mode string) task.PredicateRef {
	contract := ccrContract(previousPredicateVersion, claude.PreviousNativeContractForMode(mode), false)
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: previousPredicateVersion, SHA256: task.ComputeSHA256([]byte(contract))}
}

// HistoricalReference identifies CCR evidence sealed under the pre-issue47
// Claude parser. It remains registered solely so sealed tasks can replay.
func HistoricalReference(mode string) task.PredicateRef {
	contract := ccrContract(historicalPredicateVersion, claude.HistoricalNativeContractForMode(mode), false)
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: historicalPredicateVersion, SHA256: task.ComputeSHA256([]byte(contract))}
}

func ccrContract(version, claudeContract string, preservesRoute bool) string {
	if claudeContract == "" {
		return ""
	}
	contract := version + "\nprovider-only; explicit alias; foreground exit must succeed; session provider is ccr:print"
	if preservesRoute {
		contract += "; exact continuations retain the recorded CCR route"
	}
	return contract + "\n" + claudeContract
}

func RuntimeRequirements() commonprovider.RuntimeCapability {
	return runtimeRequirements()
}

// runtimeRequirements covers the arguments used by every CCR launch. The
// route-preserving continuation flag is selected from the supervised launch
// help projection: older CCR releases retain the explicit model/auth route.
func runtimeRequirements() commonprovider.RuntimeCapability {
	return commonprovider.RuntimeCapability{VersionArgs: []string{"version"}, HelpArgs: []string{"launch"}, RequiredFlags: []string{"--model", "--auth-mode", "--print", "--permission-mode"}}
}

func preparationForReference(reference task.PredicateRef, mode string) (preparationContract, error) {
	if err := task.ValidatePredicateRef(reference); err != nil {
		return preparationContract{}, err
	}
	switch {
	case reference.Equal(Reference(mode)):
		return preparationContract{launch: resumeRoutePreparation, inspection: routeInspection}, nil
	case reference.Equal(modeledReference(mode)):
		return preparationContract{launch: legacyPreparation, inspection: routeInspection}, nil
	case reference.Equal(previousReference(mode)), reference.Equal(HistoricalReference(mode)):
		return preparationContract{launch: legacyPreparation, inspection: legacyInspection}, nil
	default:
		return preparationContract{}, task.ErrIdentityMismatch
	}
}

func Registration() commonprovider.Registration {
	interpreters := make([]predicate.Interpreter, 0, 8)
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		interpreter, err := claude.NewStreamInterpreterWithRecovery(Reference(mode), modeledReference(mode))
		if err != nil {
			panic(err)
		} // Immutable compiled contract.
		interpreters = append(interpreters, interpreter)
	}
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		interpreter, err := claude.NewStreamInterpreterWithRecovery(modeledReference(mode), previousReference(mode))
		if err != nil {
			panic(err)
		} // Immutable compiled contract.
		interpreters = append(interpreters, interpreter)
	}
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		interpreter, err := claude.NewStreamInterpreterWithRecovery(previousReference(mode), HistoricalReference(mode))
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
		ValidateContinuation: validateContinuation,
		Models:               ModelDiscovery, Interpreters: interpreters,
	}
}

func validateContinuation(predecessor task.TaskRecord, predecessorMeta task.MetaRecord, successor task.TaskRecord) error {
	contract, err := preparationForReference(predecessorMeta.Predicate, predecessor.Mode)
	if err != nil {
		return err
	}
	if contract.launch == resumeRoutePreparation && predecessor.RequestedConfig.Model != successor.RequestedConfig.Model {
		return fmt.Errorf("%w: ccr exact-session continuation cannot change --model", commonprovider.ErrUnsupportedOption)
	}
	return nil
}
