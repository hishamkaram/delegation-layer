package opencode

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

// PrepareCandidate performs static preparation for OpenCode's native profile.
// The provider owns configuration, extensions, authentication, and permission
// resolution; the core still owns the supervised runtime capability probe.
func PrepareCandidate(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	return prepareCandidate(request, true, nil)
}

// PrepareExistingCandidate selects the preparation contract recorded by an
// already admitted task. New native tasks use the native profile; historical
// tasks retain the isolated policy inspection and launch flags they recorded.
func PrepareExistingCandidate(request task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	native := meta.EffectiveConfig.Policy != nil && meta.EffectiveConfig.Policy.ProfileRevision == commonprovider.NativeProfileRevision
	return prepareCandidate(request, native, &meta)
}

func prepareCandidate(request task.TaskRecord, native bool, meta *task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	arguments, err := runArgumentsForProfile(request, native)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	environment, err := candidateEnvironment(native, meta)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	cli, err := resolveExecutable()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	launchEnvironment, err := candidateLaunchEnvironment(request, environment, native, meta)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	definition, err := candidateInspection(cli, request, launchEnvironment, native)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if !native {
		if err := validateLegacyPreparation(request, environment, cli.SHA256); err != nil {
			return commonprovider.ProfileCandidate{}, err
		}
	}
	predicateReference := candidatePredicateReference(request.Mode, native, meta)
	return commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize:      candidateFinalizer(request, arguments, launchEnvironment, cli, environment, native, predicateReference),
	}, nil
}

func candidateEnvironment(native bool, meta *task.MetaRecord) (profileEnvironment, error) {
	values := os.Environ()
	if !native && meta != nil && len(meta.Environment) > 0 {
		values = meta.Environment
	}
	environment, err := prepareProfileEnvironment(values, native)
	if err != nil {
		return profileEnvironment{}, err
	}
	if meta != nil && meta.EffectiveConfig.Policy != nil {
		// The runner restores a historical task environment before invoking
		// reconstruction. Its recorded roots remain authoritative for matching.
		environment.WritableRoots = slices.Clone(meta.EffectiveConfig.Policy.WritableRoots)
	}
	return environment, nil
}

func candidateLaunchEnvironment(request task.TaskRecord, environment profileEnvironment, native bool, meta *task.MetaRecord) ([]string, error) {
	if native {
		return slices.Clone(environment.Values), nil
	}
	if meta != nil && len(meta.Environment) > 0 {
		return slices.Clone(meta.Environment), nil
	}
	launchEnvironment, err := environmentForMode(slices.Clone(environment.Values), request.Mode, request.CanonicalCwd)
	if err != nil {
		return nil, fmt.Errorf("%w: environment policy: %w", ErrUnsupportedProfile, err)
	}
	return launchEnvironment, nil
}

func candidateInspection(cli commonprovider.CLIInfo, request task.TaskRecord, environment []string, native bool) (commonprovider.InspectionDefinition, error) {
	requirements := RuntimeRequirements()
	if !native {
		requirements = legacyRuntimeRequirements()
	}
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, environment, requirements)
	if err != nil {
		return commonprovider.InspectionDefinition{}, fmt.Errorf("%w: runtime inspection: %w", ErrUnsupportedProfile, err)
	}
	if native {
		return definition, nil
	}
	return configureInspection(definition, request.Mode, request.CanonicalCwd)
}

func validateLegacyPreparation(request task.TaskRecord, environment profileEnvironment, runtimeSHA256 string) error {
	_, err := effectivePolicy(request, environment, runtimeSHA256, "")
	return err
}

func candidatePredicateReference(mode string, native bool, meta *task.MetaRecord) task.PredicateRef {
	if native {
		return ReferenceForMode(mode)
	}
	if meta != nil && meta.Predicate != (task.PredicateRef{}) {
		return meta.Predicate
	}
	return LegacyReferenceForMode(mode)
}

func candidateFinalizer(request task.TaskRecord, arguments, launchEnvironment []string, cli commonprovider.CLIInfo, environment profileEnvironment, native bool, predicateReference task.PredicateRef) func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
	if native {
		return func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			return finalizeCandidate(request, arguments, launchEnvironment, cli, environment, data)
		}
	}
	return func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
		return finalizeLegacyCandidate(request, arguments, launchEnvironment, cli, environment, predicateReference, data)
	}
}

// finalizeCandidate accepts only the supervised runtime facts needed by the
// current native profile. Native OpenCode policy is intentionally not projected
// into admission facts.
func finalizeCandidate(request task.TaskRecord, arguments, launchEnvironment []string, cli commonprovider.CLIInfo, environment profileEnvironment, data json.RawMessage) (commonprovider.PreparedProfile, error) {
	facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
	if decodeErr != nil || facts.Runtime == nil || facts.Native != nil {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: invalid runtime inspection facts", ErrUnsupportedProfile)
	}
	runtime := *facts.Runtime
	if runtime.Executable != cli.Path || runtime.SHA256 != cli.SHA256 {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: runtime executable changed during inspection", ErrUnsupportedProfile)
	}
	approval, err := nativeApproval(request.Mode)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	effective, err := commonprovider.NativeEffectiveConfig(request, environment.WritableRoots, runtime.SHA256, approval)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	prepared := commonprovider.PreparedProfile{
		Plan: execution.Plan{
			Executable: cli.Path, ExecutableSHA256: cli.SHA256, Arguments: cloneArguments(arguments), Directory: request.CanonicalCwd,
			Environment: slices.Clone(launchEnvironment), Predicate: ReferenceForMode(request.Mode),
		},
		ObservedVersion: runtime.Version,
		Effective:       task.CloneEffectiveConfig(effective),
		WritableRoots:   slices.Clone(environment.WritableRoots),
		Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return NewIdentityObserver(request.TaskID, expected, record)
		},
	}
	if validateErr := prepared.Validate(request); validateErr != nil {
		return commonprovider.PreparedProfile{}, validateErr
	}
	return prepared, nil
}

func finalizeLegacyCandidate(request task.TaskRecord, arguments, launchEnvironment []string, cli commonprovider.CLIInfo, environment profileEnvironment, predicateReference task.PredicateRef, data json.RawMessage) (commonprovider.PreparedProfile, error) {
	facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
	if decodeErr != nil || facts.Runtime == nil || len(facts.Native) == 0 {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: invalid runtime inspection facts", ErrUnsupportedProfile)
	}
	nativeConfigSHA256, policyErr := nativePolicyDigest(request.Mode, facts.Native)
	if policyErr != nil {
		return commonprovider.PreparedProfile{}, policyErr
	}
	runtime := *facts.Runtime
	if runtime.Executable != cli.Path || runtime.SHA256 != cli.SHA256 {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: runtime executable changed during inspection", ErrUnsupportedProfile)
	}
	effective, effectiveErr := effectivePolicy(request, environment, runtime.SHA256, nativeConfigSHA256)
	if effectiveErr != nil {
		return commonprovider.PreparedProfile{}, effectiveErr
	}
	prepared := commonprovider.PreparedProfile{
		Plan: execution.Plan{
			Executable: cli.Path, ExecutableSHA256: cli.SHA256, Arguments: cloneArguments(arguments), Directory: request.CanonicalCwd,
			Environment: slices.Clone(launchEnvironment), Predicate: predicateReference,
		},
		ObservedVersion: runtime.Version,
		Effective:       task.CloneEffectiveConfig(effective),
		WritableRoots:   slices.Clone(environment.WritableRoots),
		Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return NewIdentityObserver(request.TaskID, expected, record)
		},
	}
	if validateErr := prepared.Validate(request); validateErr != nil {
		return commonprovider.PreparedProfile{}, validateErr
	}
	return prepared, nil
}

func nativeApproval(mode string) (string, error) {
	switch mode {
	case ModeReadOnly:
		return nativeReadOnlyAgent, nil
	case ModeWorkspaceWrite:
		return nativeWorkspaceWriteAgent + ":auto", nil
	default:
		return "", fmt.Errorf("%w: unsupported OpenCode native permission mode", ErrUnsupportedProfile)
	}
}

func nativePolicyDigest(mode string, native json.RawMessage) (string, error) {
	switch mode {
	case ModeReadOnly:
		digest, err := validateReadOnlyPolicyFacts(native)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
		}
		return digest, nil
	case ModeWorkspaceWrite:
		digest, err := validateWorkspaceWritePolicyFacts(native)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
		}
		return digest, nil
	default:
		return "", fmt.Errorf("%w: unsupported OpenCode policy mode", ErrUnsupportedProfile)
	}
}

func configureInspection(definition commonprovider.InspectionDefinition, mode, workspace string) (commonprovider.InspectionDefinition, error) {
	var projector func([]byte) (json.RawMessage, error)
	switch mode {
	case ModeReadOnly:
		projector = projectReadOnlyConfig
	case ModeWorkspaceWrite:
		projector = func(native []byte) (json.RawMessage, error) {
			return projectWorkspaceWriteConfig(native, workspace)
		}
	default:
		return commonprovider.InspectionDefinition{}, fmt.Errorf("%w: unsupported OpenCode policy mode", ErrUnsupportedProfile)
	}
	definition.Arguments = []string{"debug", "config", "--pure"}
	definition.Project = projector
	snapshot, _, err := definition.Snapshot()
	if err != nil {
		return commonprovider.InspectionDefinition{}, fmt.Errorf("%w: %s policy inspection: %w", ErrUnsupportedProfile, mode, err)
	}
	return snapshot, nil
}

func effectivePolicy(request task.TaskRecord, environment profileEnvironment, runtimeSHA256, nativeConfigSHA256 string) (task.EffectiveConfig, error) {
	if err := task.ValidateSHA256(runtimeSHA256); err != nil {
		return task.EffectiveConfig{}, fmt.Errorf("%w: runtime identity: %w", ErrUnsupportedProfile, err)
	}
	if nativeConfigSHA256 != "" {
		if err := task.ValidateSHA256(nativeConfigSHA256); err != nil {
			return task.EffectiveConfig{}, fmt.Errorf("%w: native policy identity: %w", ErrUnsupportedProfile, err)
		}
	}
	details := &task.PolicyDetails{
		ProfileRevision: ProfileRevision,
		RuntimeSHA256:   runtimeSHA256,
		Workspace:       request.CanonicalCwd,
		WritableRoots:   slices.Clone(environment.WritableRoots),
	}
	effective := task.EffectiveConfig{
		Containment: request.Mode,
		Approval:    "direct-cli",
		Policy:      details,
	}
	encoded, err := task.MarshalCanonical(struct {
		Containment        string              `json:"containment"`
		Approval           string              `json:"approval"`
		Policy             *task.PolicyDetails `json:"policy,omitempty"`
		NativeConfigSHA256 string              `json:"native_config_sha256,omitempty"`
	}{
		Containment:        effective.Containment,
		Approval:           effective.Approval,
		Policy:             effective.Policy,
		NativeConfigSHA256: nativeConfigSHA256,
	})
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	effective.Digest = task.ComputeSHA256(encoded)
	if err = task.ValidateEffectiveConfig(effective); err != nil {
		return task.EffectiveConfig{}, err
	}
	return effective, nil
}
