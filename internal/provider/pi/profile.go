package pi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareCandidate performs static command construction and describes the
// shared supervised runtime capability probe. The finalizer consumes only the
// nonsecret facts produced by that probe.
func PrepareCandidate(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	return prepareCandidate(request, nativeCurrentProfile, task.PredicateRef{}, os.Environ(), true)
}

// PrepareExistingCandidate retains the launch and policy contract recorded by
// an already admitted task. New native tasks use the native profile; older
// records continue through the legacy isolated preparation path.
func PrepareExistingCandidate(request task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	profile := legacyProfile
	if meta.EffectiveConfig.Policy != nil && meta.EffectiveConfig.Policy.ProfileRevision == commonprovider.NativeProfileRevision {
		profile = nativeCurrentProfile
		if historicalNativePredicate(meta.Predicate) {
			profile = nativeHistoricalProfile
		}
	}
	environment := os.Environ()
	if meta.EnvironmentRecorded {
		environment = slices.Clone(meta.Environment)
	}
	return prepareCandidate(request, profile, meta.Predicate, environment, meta.RuntimePreflightSHA256 != "" && profile == nativeCurrentProfile)
}

type profileKind uint8

const (
	legacyProfile profileKind = iota
	nativeHistoricalProfile
	nativeCurrentProfile
)

func prepareCandidate(request task.TaskRecord, profile profileKind, storedPredicate task.PredicateRef, environmentValues []string, allowStaticAdmit bool) (commonprovider.ProfileCandidate, error) {
	native := profile != legacyProfile
	environment, err := prepareCandidateEnvironment(request, profile, environmentValues)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	arguments, err := profileArguments(request, profile, environment)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	arguments, err = historicalArguments(arguments, request, environment, profile)
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
	requirements := profileRuntimeRequirements(profile, request.Mode, storedPredicate)
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, environment.Values, requirements)
	if err != nil {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: runtime inspection: %w", ErrUnsupportedProfile, err)
	}
	var capability string
	if profile == nativeCurrentProfile && allowStaticAdmit {
		capability, err = commonprovider.RuntimeCapabilityDigest(definition)
		if err != nil {
			return commonprovider.ProfileCandidate{}, err
		}
	}
	predicateReference := profilePredicateReference(native, request.Mode, storedPredicate)
	candidate := commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize: func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			return finalizePreparedCandidate(request, native, arguments, environment, cli, legacyEffective, predicateReference, data)
		},
	}
	if capability != "" {
		candidate.Admit = func() (commonprovider.PreparedProfile, error) {
			return buildPreparedCandidate(request, true, arguments, environment, cli, legacyEffective, predicateReference, "", cli.SHA256, capability)
		}
	}
	return candidate, nil
}

func prepareCandidateEnvironment(request task.TaskRecord, profile profileKind, environmentValues []string) (profileEnvironment, error) {
	native := profile != legacyProfile
	environment, err := prepareProfileEnvironment(environmentValues, native)
	if err != nil {
		return profileEnvironment{}, err
	}
	if profile != nativeCurrentProfile || request.Mode != ModeReadOnly {
		return environment, nil
	}
	initialSessionDir := environment.SessionDir
	baseSessionDir, err := config.CanonicalizePath(filepath.Join(environment.AgentDir, "sessions"))
	if err != nil {
		return profileEnvironment{}, fmt.Errorf("%w: resolve Pi default session directory: %w", ErrUnsupportedProfile, err)
	}
	environment.SessionDir, err = resolveNativeSessionDirectory(environment, request.CanonicalCwd)
	if err != nil {
		return profileEnvironment{}, err
	}
	if initialSessionDir != baseSessionDir {
		environment.WritableRoots = slices.DeleteFunc(environment.WritableRoots, func(root string) bool { return root == initialSessionDir })
	}
	if !pathContains(baseSessionDir, environment.SessionDir) {
		environment.WritableRoots = append(environment.WritableRoots, environment.SessionDir)
	}
	environment.WritableRoots = append(environment.WritableRoots, baseSessionDir)
	slices.Sort(environment.WritableRoots)
	environment.WritableRoots = slices.Compact(environment.WritableRoots)
	// Pin the settings-derived directory in the admitted launch environment
	// so retries and continuations reuse the exact session location.
	environment.Values = pinSessionDirectory(environment.Values, environment.SessionDir)
	return environment, nil
}

func finalizePreparedCandidate(request task.TaskRecord, native bool, arguments []string, environment profileEnvironment, cli commonprovider.CLIInfo, legacyEffective task.EffectiveConfig, predicateReference task.PredicateRef, data json.RawMessage) (commonprovider.PreparedProfile, error) {
	facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
	if decodeErr != nil || facts.Runtime == nil || facts.Native != nil {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: invalid runtime inspection facts", ErrUnsupportedProfile)
	}
	runtime := *facts.Runtime
	if runtime.Executable != cli.Path || runtime.SHA256 != cli.SHA256 {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: runtime executable changed during inspection", ErrUnsupportedProfile)
	}
	return buildPreparedCandidate(request, native, arguments, environment, cli, legacyEffective, predicateReference, runtime.Version, runtime.SHA256, "")
}

func profileArguments(request task.TaskRecord, profile profileKind, environment profileEnvironment) ([]string, error) {
	switch profile {
	case nativeCurrentProfile:
		return jsonArgumentsForProfileWithSessionDir(request, true, true, environment.SessionDir)
	case nativeHistoricalProfile:
		return jsonArgumentsForProfile(request, true, false)
	case legacyProfile:
		return legacyJSONArguments(request)
	default:
		return nil, fmt.Errorf("%w: unknown Pi profile kind", ErrUnsupportedProfile)
	}
}

func historicalArguments(arguments []string, request task.TaskRecord, environment profileEnvironment, profile profileKind) ([]string, error) {
	if profile == nativeCurrentProfile && request.Mode == ModeReadOnly {
		if err := validateStartupMigrations(request.CanonicalCwd, environment.AgentDir, environment.SessionDir); err != nil {
			return nil, err
		}
	}
	if profile != legacyProfile {
		return arguments, nil
	}
	if err := validateStartupMigrations(request.CanonicalCwd, environment.AgentDir, environment.SessionDir); err != nil {
		return nil, err
	}
	return append(arguments, "--session-dir", environment.SessionDir), nil
}

func historicalEffectiveConfig(request task.TaskRecord, environment profileEnvironment, runtimeSHA256 string, native bool) (task.EffectiveConfig, error) {
	if native {
		return task.EffectiveConfig{}, nil
	}
	sources, err := inspectPolicySources(environment.AgentDir, request.CanonicalCwd)
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	return effectivePolicy(request, environment, runtimeSHA256, sources)
}

func profileRuntimeRequirements(profile profileKind, mode string, storedPredicate task.PredicateRef) commonprovider.RuntimeCapability {
	switch profile {
	case nativeCurrentProfile:
		return RuntimeRequirements(mode)
	case nativeHistoricalProfile:
		if storedPredicate.Equal(workspaceWriteV7Reference()) {
			return workspaceWriteV7RuntimeRequirements()
		}
		return historicalNativeRuntimeRequirements()
	case legacyProfile:
		return legacyRuntimeRequirements()
	default:
		return commonprovider.RuntimeCapability{}
	}
}

func buildPreparedCandidate(request task.TaskRecord, native bool, arguments []string, environment profileEnvironment, cli commonprovider.CLIInfo, legacyEffective task.EffectiveConfig, predicateReference task.PredicateRef, observedVersion, runtimeSHA256, preflightSHA256 string) (commonprovider.PreparedProfile, error) {
	effective, err := finalizedEffectiveConfig(request, native, environment, legacyEffective, runtimeSHA256)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	prepared := commonprovider.PreparedProfile{
		Plan: execution.Plan{
			Executable: cli.Path, ExecutableSHA256: cli.SHA256, Arguments: slices.Clone(arguments), Directory: request.CanonicalCwd,
			Environment: slices.Clone(environment.Values), Predicate: predicateReference,
		},
		ObservedVersion:        observedVersion,
		RuntimePreflightSHA256: preflightSHA256,
		Effective:              task.CloneEffectiveConfig(effective),
		WritableRoots:          slices.Clone(environment.WritableRoots),
		Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return NewIdentityObserver(expected, record)
		},
	}
	if err := prepared.ValidateAdmission(request); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return prepared, nil
}

func finalizedEffectiveConfig(request task.TaskRecord, native bool, environment profileEnvironment, legacyEffective task.EffectiveConfig, runtimeSHA256 string) (task.EffectiveConfig, error) {
	if native {
		approval, err := nativeApproval(request.Mode)
		if err != nil {
			return task.EffectiveConfig{}, err
		}
		return commonprovider.NativeEffectiveConfig(request, environment.WritableRoots, runtimeSHA256, approval)
	}
	return legacyEffective, nil
}

func nativeApproval(mode string) (string, error) {
	switch mode {
	case ModeReadOnly:
		return "read-only", nil
	case ModeWorkspaceWrite:
		return "provider-native", nil
	default:
		return "", fmt.Errorf("%w: unsupported Pi native permission mode", ErrUnsupportedProfile)
	}
}

func profilePredicateReference(native bool, mode string, stored task.PredicateRef) task.PredicateRef {
	if !native {
		return LegacyReference()
	}
	switch {
	case mode == ModeReadOnly && (stored.Equal(readOnlyV2Reference()) || stored.Equal(readOnlyV3Reference()) || stored.Equal(readOnlyV4Reference()) || stored.Equal(readOnlyV5Reference())):
		return stored
	case mode == ModeWorkspaceWrite && (stored.Equal(workspaceWriteV3Reference()) || stored.Equal(workspaceWriteV4Reference()) || stored.Equal(workspaceWriteV5Reference()) || stored.Equal(workspaceWriteV7Reference())):
		return stored
	}
	return ReferenceForMode(mode)
}

func historicalNativePredicate(reference task.PredicateRef) bool {
	return reference.Equal(readOnlyV2Reference()) ||
		reference.Equal(readOnlyV3Reference()) ||
		reference.Equal(readOnlyV4Reference()) ||
		reference.Equal(readOnlyV5Reference()) ||
		reference.Equal(workspaceWriteV3Reference()) ||
		reference.Equal(workspaceWriteV4Reference()) ||
		reference.Equal(workspaceWriteV5Reference()) ||
		reference.Equal(workspaceWriteV7Reference())
}
