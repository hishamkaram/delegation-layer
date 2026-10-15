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
	return prepareCandidate(request, true, task.PredicateRef{}, Provider, false, true)
}

// PrepareReviewCandidate prepares the read-only native Codex review operation.
func PrepareReviewCandidate(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	return prepareCandidate(request, true, task.PredicateRef{}, ReviewProvider, true, true)
}

// PrepareExistingCandidate retains the launch and policy contract recorded by
// an already admitted task. New native tasks use the native profile; older
// records continue through the legacy isolated preparation path.
func PrepareExistingCandidate(request task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	native := meta.EffectiveConfig.Policy != nil && meta.EffectiveConfig.Policy.ProfileRevision == commonprovider.NativeProfileRevision
	return prepareCandidate(request, native, meta.Predicate, Provider, false, meta.RuntimePreflightSHA256 != "" && native)
}

// PrepareExistingReviewCandidate reconstructs a previously admitted review.
func PrepareExistingReviewCandidate(request task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	return prepareCandidate(request, true, meta.Predicate, ReviewProvider, true, meta.RuntimePreflightSHA256 != "")
}

func prepareCandidate(request task.TaskRecord, native bool, storedPredicate task.PredicateRef, providerID string, review, allowStaticAdmit bool) (commonprovider.ProfileCandidate, error) {
	if request.RequestedConfig.Interactive && !storedPredicate.Equal(task.PredicateRef{}) &&
		!storedPredicate.Equal(interactionReference(request.Mode)) &&
		!storedPredicate.Equal(historicalInteractionReference(request.Mode)) {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: stored Codex predicate is not interactive", ErrUnsupportedProfile)
	}
	predicateReference := profilePredicateReference(request.Mode, storedPredicate, providerID)
	if request.RequestedConfig.Interactive {
		if storedPredicate.Equal(task.PredicateRef{}) {
			predicateReference = interactionReference(request.Mode)
		} else {
			predicateReference = storedPredicate
		}
	}
	skipGitCheck := native && predicateReference.Equal(ReferenceForMode(request.Mode))
	arguments, output, err := profileArguments(request, native, review, skipGitCheck)
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
	requirements := profileRuntimeRequirements(request, native, review, skipGitCheck)
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, environment.Values, requirements)
	if err != nil {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: runtime inspection: %w", ErrUnsupportedProfile, err)
	}
	capability, err := staticCapability(definition, native, allowStaticAdmit)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	candidate := commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize: func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			return finalizePreparedCandidate(request, native, arguments, output, environment, cli, legacyEffective, predicateReference, providerID, review, data)
		},
	}
	if capability != "" {
		candidate.Admit = func() (commonprovider.PreparedProfile, error) {
			return buildPreparedCandidate(request, true, arguments, output, environment, cli, legacyEffective, predicateReference, providerID, review, "", cli.SHA256, capability)
		}
	}
	return candidate, nil
}

func staticCapability(definition commonprovider.InspectionDefinition, native, allowStaticAdmit bool) (string, error) {
	if !native || !allowStaticAdmit {
		return "", nil
	}
	return commonprovider.RuntimeCapabilityDigest(definition)
}

func finalizePreparedCandidate(request task.TaskRecord, native bool, arguments []string, output int, environment profileEnvironment, cli commonprovider.CLIInfo, legacyEffective task.EffectiveConfig, predicateReference task.PredicateRef, providerID string, review bool, data json.RawMessage) (commonprovider.PreparedProfile, error) {
	facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
	if decodeErr != nil || facts.Runtime == nil || facts.Native != nil {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: invalid runtime inspection facts", ErrUnsupportedProfile)
	}
	runtime := *facts.Runtime
	if runtime.Executable != cli.Path || runtime.SHA256 != cli.SHA256 {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: runtime executable changed during inspection", ErrUnsupportedProfile)
	}
	return buildPreparedCandidate(request, native, arguments, output, environment, cli, legacyEffective, predicateReference, providerID, review, runtime.Version, runtime.SHA256, "")
}

func profileArguments(request task.TaskRecord, native, review, skipGitCheck bool) ([]string, int, error) {
	if review {
		return reviewArguments(request)
	}
	if request.RequestedConfig.Interactive {
		return []string{"app-server", "--listen", "stdio://", "--enable", "default_mode_request_user_input"}, -1, nil
	}
	return execArgumentsForProfile(request, native, skipGitCheck)
}

func historicalEffectiveConfig(request task.TaskRecord, environment profileEnvironment, runtimeSHA256 string, native bool) (task.EffectiveConfig, error) {
	if native {
		return task.EffectiveConfig{}, nil
	}
	return effectivePolicy(request, environment, time.Now(), managedSources, runtimeSHA256)
}

func profileRuntimeRequirements(request task.TaskRecord, native, review, skipGitCheck bool) commonprovider.RuntimeCapability {
	if review {
		return reviewRuntimeRequirementsForRequest(request)
	}
	if request.RequestedConfig.Interactive {
		return commonprovider.RuntimeCapability{HelpArgs: []string{"app-server"}, RequiredFlags: []string{"--listen", "--enable"}}
	}
	if native {
		return runtimeRequirementsForRequest(request, nativeRuntimeRequirements(skipGitCheck))
	}
	return runtimeRequirementsForRequest(request, legacyRuntimeRequirements())
}

func buildPreparedCandidate(request task.TaskRecord, native bool, arguments []string, output int, environment profileEnvironment, cli commonprovider.CLIInfo, legacyEffective task.EffectiveConfig, predicateReference task.PredicateRef, providerID string, review bool, observedVersion, runtimeSHA256, preflightSHA256 string) (commonprovider.PreparedProfile, error) {
	effective, err := finalizedEffectiveConfig(request, native, environment, legacyEffective, runtimeSHA256)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	plan := execution.Plan{
		Executable: cli.Path, ExecutableSHA256: cli.SHA256, Arguments: slices.Clone(arguments), Directory: request.CanonicalCwd,
		Environment: slices.Clone(environment.Values), Predicate: predicateReference,
		OutputWriterContract: OutputWriterContract,
	}
	if output >= 0 {
		plan.OutputArtifacts = []task.OutputArtifact{{Name: OutputName, ArgumentIndex: output}}
	}
	if review {
		plan.OutputWriterContract = ""
	}
	prepared := commonprovider.PreparedProfile{
		Plan:                   plan,
		ObservedVersion:        observedVersion,
		RuntimePreflightSHA256: preflightSHA256,
		Effective:              task.CloneEffectiveConfig(effective), WritableRoots: slices.Clone(environment.WritableRoots),
		Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return newIdentityObserver(providerID, request.TaskID, expected, record)
		},
	}
	if request.RequestedConfig.Interactive && !review {
		prepared.Plan.OutputArtifacts = nil
		prepared.Plan.OutputWriterContract = ""
		prepared.Identity = nil
		prepared.Interaction = func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
			return newAppServerProtocolWithCapture(request, expected, record, predicateReference.Equal(interactionReference(request.Mode)))
		}
	}
	if err := prepared.ValidateAdmission(request); err != nil {
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

func profilePredicateReference(mode string, stored task.PredicateRef, providerID string) task.PredicateRef {
	if !stored.Equal(task.PredicateRef{}) {
		return stored
	}
	if providerID == ReviewProvider {
		return reviewReference()
	}
	return ReferenceForMode(mode)
}
