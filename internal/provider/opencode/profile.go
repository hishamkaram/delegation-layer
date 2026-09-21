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

// PrepareCandidate performs static preparation and describes the supervised
// runtime capability probe. The core owns the probe process and supplies only
// nonsecret runtime facts to the finalizer.
func PrepareCandidate(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	arguments, err := runArguments(request)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	environment, err := prepareEnvironment(os.Environ())
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	cli, err := resolveExecutable()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	launchEnvironment, err := environmentForMode(environment.Values, request.Mode, request.CanonicalCwd)
	if err != nil {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: environment policy: %w", ErrUnsupportedProfile, err)
	}
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, launchEnvironment, RuntimeRequirements())
	if err != nil {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: runtime inspection: %w", ErrUnsupportedProfile, err)
	}
	definition, err = configureInspection(definition, request.Mode, request.CanonicalCwd)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if _, policyErr := effectivePolicy(request, environment, cli.SHA256, ""); policyErr != nil {
		return commonprovider.ProfileCandidate{}, policyErr
	}
	return commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize: func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			return finalizeCandidate(request, arguments, launchEnvironment, cli, environment, data)
		},
	}, nil
}

// PrepareExistingCandidate reconstructs an admitted task from its recorded
// launch environment. This keeps historical OpenCode tasks bound to the
// environment and writable roots that were admitted with them while new tasks
// use the current isolated configuration policy above.
func PrepareExistingCandidate(request task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	if len(meta.Environment) == 0 {
		return PrepareCandidate(request)
	}
	arguments, err := runArguments(request)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	environment, err := prepareEnvironment(meta.Environment)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if meta.EffectiveConfig.Policy != nil {
		// Keep the exact writable roots admitted with an older task. The
		// current environment policy may add the isolated config root, but
		// recovery must continue to match the immutable historical profile.
		environment.WritableRoots = slices.Clone(meta.EffectiveConfig.Policy.WritableRoots)
	}
	cli, err := resolveExecutable()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	launchEnvironment := slices.Clone(meta.Environment)
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, launchEnvironment, RuntimeRequirements())
	if err != nil {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: runtime inspection: %w", ErrUnsupportedProfile, err)
	}
	definition, err = configureInspection(definition, request.Mode, request.CanonicalCwd)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if _, policyErr := effectivePolicy(request, environment, cli.SHA256, ""); policyErr != nil {
		return commonprovider.ProfileCandidate{}, policyErr
	}
	return commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize: func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			return finalizeCandidate(request, arguments, launchEnvironment, cli, environment, data)
		},
	}, nil
}

func finalizeCandidate(request task.TaskRecord, arguments, launchEnvironment []string, cli commonprovider.CLIInfo, environment profileEnvironment, data json.RawMessage) (commonprovider.PreparedProfile, error) {
	facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
	if decodeErr != nil || facts.Runtime == nil {
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
			Executable: cli.Path, Arguments: cloneArguments(arguments), Directory: request.CanonicalCwd,
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
