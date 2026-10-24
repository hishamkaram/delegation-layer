package opencode

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareCandidate performs static preparation for OpenCode's native ACP
// profile. The provider owns configuration, extensions, authentication, and
// permission resolution; the core still owns the supervised runtime
// capability probe and the single stdio transport. environment is the caller's
// selected launch environment.
func PrepareCandidate(request task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	return prepareCandidate(request, environment, dialectACP, nil, acpReferenceForMode(request.Mode), true)
}

// PrepareExistingCandidate selects the preparation contract recorded by an
// already admitted task. Its recorded profile revision selects the launch
// dialect, so historical tasks retain the policy inspection, runtime
// capability probe, launch flags, and predicate they recorded. The caller's
// environment resolves the executable, and it is the launch environment only
// for a record that saved none.
func PrepareExistingCandidate(request task.TaskRecord, meta task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	dialect, err := dialectForRecord(meta)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	reference, err := dialect.recordedPredicate(request.Mode, meta.Predicate)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return prepareCandidate(request, environment, dialect, &meta, reference, meta.RuntimePreflightSHA256 != "" && dialect.native())
}

// prepareCandidate prepares dialect for request. meta, when present, supplies
// the recorded environment and writable roots; scope resolves the executable
// and is the launch environment only when meta saved none. predicateReference
// is the output contract selected by the caller.
func prepareCandidate(request task.TaskRecord, scope []string, dialect launchDialect, meta *task.MetaRecord, predicateReference task.PredicateRef, allowStaticAdmit bool) (commonprovider.ProfileCandidate, error) {
	native := dialect.native()
	arguments, err := runArguments(request, dialect)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	environment, err := candidateEnvironment(scope, native, meta)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	cli, err := resolveExecutable(scope)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	launchEnvironment, err := candidateLaunchEnvironment(request, environment, native, meta)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	definition, err := candidateInspection(cli, request, launchEnvironment, dialect)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if !native {
		err = validateLegacyPreparation(request, environment, cli.SHA256)
		if err != nil {
			return commonprovider.ProfileCandidate{}, err
		}
	}
	var capability string
	if native && allowStaticAdmit {
		capability, err = commonprovider.RuntimeCapabilityDigest(definition)
		if err != nil {
			return commonprovider.ProfileCandidate{}, err
		}
	}
	candidate := commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize:      candidateFinalizer(request, arguments, launchEnvironment, cli, environment, dialect, predicateReference),
	}
	if capability != "" {
		candidate.Admit = func() (commonprovider.PreparedProfile, error) {
			return buildNativePreparedProfile(request, arguments, launchEnvironment, cli, environment, dialect, predicateReference, "", cli.SHA256, capability)
		}
	}
	return candidate, nil
}

func candidateEnvironment(callerEnvironment []string, native bool, meta *task.MetaRecord) (profileEnvironment, error) {
	values := callerEnvironment
	if meta != nil {
		if recorded, ok := commonprovider.RecordedEnvironment(*meta); ok {
			values = recorded
		}
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
	if meta != nil {
		if recorded, ok := commonprovider.RecordedEnvironment(*meta); ok {
			return recorded, nil
		}
	}
	launchEnvironment, err := environmentForMode(slices.Clone(environment.Values), request.Mode, request.CanonicalCwd)
	if err != nil {
		return nil, fmt.Errorf("%w: environment policy: %w", ErrUnsupportedProfile, err)
	}
	return launchEnvironment, nil
}

func candidateInspection(cli commonprovider.CLIInfo, request task.TaskRecord, environment []string, dialect launchDialect) (commonprovider.InspectionDefinition, error) {
	requirements, err := dialect.runtimeRequirements()
	if err != nil {
		return commonprovider.InspectionDefinition{}, err
	}
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, environment, requirements)
	if err != nil {
		return commonprovider.InspectionDefinition{}, fmt.Errorf("%w: runtime inspection: %w", ErrUnsupportedProfile, err)
	}
	if dialect.native() {
		return definition, nil
	}
	return configureInspection(definition, request.Mode, request.CanonicalCwd)
}

func validateLegacyPreparation(request task.TaskRecord, environment profileEnvironment, runtimeSHA256 string) error {
	_, err := effectivePolicy(request, environment, runtimeSHA256, "")
	return err
}

func candidateFinalizer(request task.TaskRecord, arguments, launchEnvironment []string, cli commonprovider.CLIInfo, environment profileEnvironment, dialect launchDialect, predicateReference task.PredicateRef) func(json.RawMessage, time.Time) (commonprovider.PreparedProfile, error) {
	if dialect.native() {
		return func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			return finalizeCandidate(request, arguments, launchEnvironment, cli, environment, dialect, predicateReference, data)
		}
	}
	return func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
		return finalizeLegacyCandidate(request, arguments, launchEnvironment, cli, environment, predicateReference, data)
	}
}

// finalizeCandidate accepts only the supervised runtime facts needed by the
// current native profile. Native OpenCode policy is intentionally not projected
// into admission facts.
func finalizeCandidate(request task.TaskRecord, arguments, launchEnvironment []string, cli commonprovider.CLIInfo, environment profileEnvironment, dialect launchDialect, predicateReference task.PredicateRef, data json.RawMessage) (commonprovider.PreparedProfile, error) {
	facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
	if decodeErr != nil || facts.Runtime == nil || facts.Native != nil {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: invalid runtime inspection facts", ErrUnsupportedProfile)
	}
	runtime := *facts.Runtime
	if runtime.Executable != cli.Path || runtime.SHA256 != cli.SHA256 {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: runtime executable changed during inspection", ErrUnsupportedProfile)
	}
	return buildNativePreparedProfile(request, arguments, launchEnvironment, cli, environment, dialect, predicateReference, runtime.Version, runtime.SHA256, "")
}

func buildNativePreparedProfile(request task.TaskRecord, arguments, launchEnvironment []string, cli commonprovider.CLIInfo, environment profileEnvironment, dialect launchDialect, predicateReference task.PredicateRef, observedVersion, runtimeSHA256, preflightSHA256 string) (commonprovider.PreparedProfile, error) {
	approval, err := dialect.approval(request.Mode)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	effective, err := dialect.nativeEffectiveConfig(request, environment.WritableRoots, runtimeSHA256, approval)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	prepared := commonprovider.PreparedProfile{
		Plan: execution.Plan{
			Executable: cli.Path, ExecutableSHA256: cli.SHA256, Arguments: cloneArguments(arguments), Directory: request.CanonicalCwd,
			Environment: slices.Clone(launchEnvironment), Predicate: predicateReference,
		},
		ObservedVersion:        observedVersion,
		RuntimePreflightSHA256: preflightSHA256,
		Effective:              task.CloneEffectiveConfig(effective),
		WritableRoots:          slices.Clone(environment.WritableRoots),
	}
	if dialect == dialectACP {
		// The ACP protocol is the sole stdout observer and records the exact
		// native session itself, so the profile has no identity observer.
		prepared.Protocol = func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.Protocol, error) {
			return newACPProtocol(request, expected, record)
		}
	} else {
		prepared.Identity = func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return NewIdentityObserver(request.TaskID, expected, record)
		}
	}
	if validateErr := prepared.ValidateAdmission(request); validateErr != nil {
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
