package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareCandidate performs static preparation only. The shared inspection
// worker owns the provider CLI capability probe before the finalizer receives
// its nonsecret runtime facts.
func PrepareCandidate(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	return prepareCandidate(request, true, task.PredicateRef{})
}

// PrepareExistingCandidate retains the launch and policy contract recorded by
// an already admitted task. New native tasks use the native profile; older
// records continue through the legacy isolated preparation path.
func PrepareExistingCandidate(request task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	native := meta.EffectiveConfig.Policy != nil && meta.EffectiveConfig.Policy.ProfileRevision == commonprovider.NativeProfileRevision
	return prepareCandidate(request, native, meta.Predicate)
}

func prepareCandidate(request task.TaskRecord, native bool, storedPredicate task.PredicateRef) (commonprovider.ProfileCandidate, error) {
	arguments, output, err := profileArguments(request, native)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	environment, err := prepareProfileEnvironment(os.Environ(), native)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	cli, err := resolveExecutable()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	legacyEffective, err := historicalEffectiveConfig(request, environment, cli.SHA256, native)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	requirements := profileRuntimeRequirements(native)
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, environment.Values, requirements)
	if err != nil {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: runtime inspection: %w", ErrUnsupportedProfile, err)
	}
	predicateReference := profilePredicateReference(native, request.Mode, storedPredicate)
	return commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize: func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			return finalizePreparedCandidate(request, native, arguments, output, environment, cli, legacyEffective, predicateReference, data)
		},
	}, nil
}

func profileArguments(request task.TaskRecord, native bool) ([]string, int, error) {
	if native {
		return execArguments(request)
	}
	return legacyExecArguments(request)
}

func historicalEffectiveConfig(request task.TaskRecord, environment profileEnvironment, runtimeSHA256 string, native bool) (task.EffectiveConfig, error) {
	if native {
		return task.EffectiveConfig{}, nil
	}
	return effectivePolicy(request, environment, time.Now(), managedSources, runtimeSHA256)
}

func profileRuntimeRequirements(native bool) commonprovider.RuntimeCapability {
	if native {
		return RuntimeRequirements()
	}
	return legacyRuntimeRequirements()
}

func finalizePreparedCandidate(request task.TaskRecord, native bool, arguments []string, output int, environment profileEnvironment, cli commonprovider.CLIInfo, legacyEffective task.EffectiveConfig, predicateReference task.PredicateRef, data json.RawMessage) (commonprovider.PreparedProfile, error) {
	facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
	if decodeErr != nil || facts.Runtime == nil || facts.Native != nil {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: invalid runtime inspection facts", ErrUnsupportedProfile)
	}
	runtime := *facts.Runtime
	if runtime.Executable != cli.Path || runtime.SHA256 != cli.SHA256 {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: runtime executable changed during inspection", ErrUnsupportedProfile)
	}
	effective, err := finalizedEffectiveConfig(request, native, environment, legacyEffective, runtime.SHA256)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	prepared := commonprovider.PreparedProfile{
		Plan: execution.Plan{
			Executable: cli.Path, Arguments: slices.Clone(arguments), Directory: request.CanonicalCwd,
			Environment: slices.Clone(environment.Values), Predicate: predicateReference,
			OutputArtifacts:      []task.OutputArtifact{{Name: OutputName, ArgumentIndex: output}},
			OutputWriterContract: OutputWriterContract,
		},
		ObservedVersion: runtime.Version, Effective: task.CloneEffectiveConfig(effective), WritableRoots: slices.Clone(environment.WritableRoots),
		Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return NewIdentityObserver(request.TaskID, expected, record)
		},
	}
	if err := prepared.Validate(request); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return prepared, nil
}

func finalizedEffectiveConfig(request task.TaskRecord, native bool, environment profileEnvironment, legacyEffective task.EffectiveConfig, runtimeSHA256 string) (task.EffectiveConfig, error) {
	if native {
		return commonprovider.NativeEffectiveConfig(request, environment.WritableRoots, runtimeSHA256, "never")
	}
	return legacyEffective, nil
}

func profilePredicateReference(native bool, mode string, stored task.PredicateRef) task.PredicateRef {
	if native || stored == (task.PredicateRef{}) {
		return ReferenceForMode(mode)
	}
	return stored
}
