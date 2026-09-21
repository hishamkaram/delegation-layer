package claude

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareCandidate resolves the native launch plan and describes the shared
// runtime capability probe. Claude owns configuration, authentication, and
// permission policy; the adapter retains only request and runtime checks.
func PrepareCandidate(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	return prepareNativeCandidate(request)
}

func prepareNativeCandidate(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	return prepareNativeCandidateWithContract(request, nil, nil, false)
}

func prepareExistingNativeCandidate(request task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	var recordedRoots []string
	if meta.EffectiveConfig.Policy != nil {
		recordedRoots = meta.EffectiveConfig.Policy.WritableRoots
	}
	historicalReadOnly := request.Mode == Mode && meta.EffectiveConfig.Approval == "dontAsk"
	return prepareNativeCandidateWithContract(request, meta.Environment, recordedRoots, historicalReadOnly)
}

func prepareNativeCandidateWithContract(request task.TaskRecord, recordedEnvironment, recordedRoots []string, historicalReadOnly bool) (commonprovider.ProfileCandidate, error) {
	arguments, inputs, err := printArguments(request)
	if historicalReadOnly {
		arguments, inputs, err = historicalNativePrintArguments(request)
	}
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	environmentValues := os.Environ()
	if recordedEnvironment != nil {
		environmentValues = recordedEnvironment
	}
	prepareEnvironmentFn := prepareNativeEnvironment
	if historicalReadOnly || recordedEnvironment != nil {
		prepareEnvironmentFn = prepareHistoricalNativeEnvironment
	}
	environment, err := prepareEnvironmentFn(environmentValues)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if len(recordedRoots) > 0 {
		environment.WritableRoots = slices.Clone(recordedRoots)
	}
	cli, err := resolveExecutable()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	environment.RuntimeSHA256 = cli.SHA256
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, environment.Values, RuntimeRequirements())
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	var definitionDigest string
	definition, definitionDigest, err = definition.Snapshot()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize: func(data json.RawMessage, now time.Time) (commonprovider.PreparedProfile, error) {
			return finalizeNativePreparedProfileWithContract(request, arguments, inputs, cli, environment, definitionDigest, data, now, historicalReadOnly)
		},
	}, nil
}

// PrepareExistingCandidate selects the launch and policy contract recorded by
// an already admitted task. Native tasks use the current provider-owned plan;
// older portable and OAuth records retain their historical branches.
func PrepareExistingCandidate(request task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	if meta.EffectiveConfig.Policy != nil && meta.EffectiveConfig.Policy.ProfileRevision == commonprovider.NativeProfileRevision {
		return prepareExistingNativeCandidate(request, meta)
	}
	if !hasLegacyNativePolicy(meta) {
		return preparePortableCandidate(request, meta.Predicate)
	}
	return prepareLegacyCandidate(request, meta.Predicate)
}

func hasLegacyNativePolicy(meta task.MetaRecord) bool {
	if meta.EffectiveConfig.Policy == nil {
		return false
	}
	for _, source := range meta.EffectiveConfig.Policy.Sources {
		if source.Kind == "native-oauth-policy-proof" && source.Present {
			return true
		}
	}
	return false
}

func preparePortableCandidate(request task.TaskRecord, predicateReference task.PredicateRef) (commonprovider.ProfileCandidate, error) {
	arguments, inputs, err := legacyPrintArguments(request)
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
	environment.RuntimeSHA256 = cli.SHA256
	sources, err := resolvePolicySources(request, environment)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if predicateReference.Adapter == "" {
		predicateReference = legacyPortableReferenceForMode(request.Mode)
	}
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, environment.Values, legacyRuntimeRequirements())
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	var definitionDigest string
	definition, definitionDigest, err = definition.Snapshot()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize: func(data json.RawMessage, now time.Time) (commonprovider.PreparedProfile, error) {
			return finalizePreparedProfile(request, arguments, inputs, cli, environment, sources, definitionDigest, data, now, predicateReference)
		},
	}, nil
}

func prepareLegacyCandidate(request task.TaskRecord, predicateReference task.PredicateRef) (commonprovider.ProfileCandidate, error) {
	arguments, inputs, err := legacyPrintArguments(request)
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
	environment.RuntimeSHA256 = cli.SHA256
	sources, err := resolvePolicySources(request, environment)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if err = validateLegacyAPIKeyInspection(inspectLegacyAPIKey(environment)); err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	definition, err := nativeInspection(environment)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	requirements := legacyRuntimeRequirements()
	definition.Runtime = &commonprovider.RuntimeProbeDefinition{
		Executable:       cli.Path,
		ExecutableSHA256: cli.SHA256,
		Directory:        request.CanonicalCwd,
		Environment:      slices.Clone(environment.Values),
		HelpArgs:         slices.Clone(requirements.HelpArgs),
		RequiredFlags:    slices.Clone(requirements.RequiredFlags),
	}
	var definitionDigest string
	definition, definitionDigest, err = definition.Snapshot()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize: func(data json.RawMessage, now time.Time) (commonprovider.PreparedProfile, error) {
			return finalizeLegacyPreparedProfile(request, arguments, inputs, cli, environment, sources, definitionDigest, predicateReference, data, now)
		},
	}, nil
}

func finalizePreparedProfile(
	request task.TaskRecord,
	arguments []string,
	inputs []task.InputFile,
	cli commonprovider.CLIInfo,
	environment profileEnvironment,
	sources []task.PolicySourceDigest,
	definitionDigest string,
	data json.RawMessage,
	now time.Time,
	predicateReferences ...task.PredicateRef,
) (commonprovider.PreparedProfile, error) {
	facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
	if decodeErr != nil || task.ValidateSHA256(definitionDigest) != nil || facts.Runtime == nil || facts.Native != nil || facts.Runtime.Executable != cli.Path || facts.Runtime.SHA256 != cli.SHA256 {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: invalid runtime inspection facts", ErrUnsupportedProfile)
	}
	runtime := *facts.Runtime
	effective, err := finalizePortablePolicy(request, environment, sources, now)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	predicateReference := legacyPortableReferenceForMode(request.Mode)
	if len(predicateReferences) > 0 && predicateReferences[0].Adapter != "" {
		predicateReference = predicateReferences[0]
	}
	prepared := commonprovider.PreparedProfile{
		Plan: execution.Plan{
			Executable: cli.Path, Arguments: slices.Clone(arguments), Directory: request.CanonicalCwd,
			Environment: slices.Clone(environment.Values), Predicate: predicateReference, InputFiles: slices.Clone(inputs),
		},
		ObservedVersion: runtime.Version, Effective: effective, WritableRoots: slices.Clone(environment.WritableRoots),
		Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return NewIdentityObserver(request.RootID, request.TaskID, expected, record)
		},
	}
	if err = prepared.Validate(request); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return prepared, nil
}

func finalizeNativePreparedProfile(
	request task.TaskRecord,
	arguments []string,
	inputs []task.InputFile,
	cli commonprovider.CLIInfo,
	environment profileEnvironment,
	definitionDigest string,
	data json.RawMessage,
	now time.Time,
) (commonprovider.PreparedProfile, error) {
	return finalizeNativePreparedProfileWithContract(request, arguments, inputs, cli, environment, definitionDigest, data, now, false)
}

func finalizeNativePreparedProfileWithContract(
	request task.TaskRecord,
	arguments []string,
	inputs []task.InputFile,
	cli commonprovider.CLIInfo,
	environment profileEnvironment,
	definitionDigest string,
	data json.RawMessage,
	_ time.Time,
	historicalReadOnly bool,
) (commonprovider.PreparedProfile, error) {
	facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
	if decodeErr != nil || task.ValidateSHA256(definitionDigest) != nil || facts.Runtime == nil || facts.Native != nil || facts.Runtime.Executable != cli.Path || facts.Runtime.SHA256 != cli.SHA256 {
		return commonprovider.PreparedProfile{}, fmt.Errorf("%w: invalid runtime inspection facts", ErrUnsupportedProfile)
	}
	runtime := *facts.Runtime
	approval := "plan"
	predicateReference := ReferenceForMode(request.Mode)
	if historicalReadOnly {
		approval = "dontAsk"
		predicateReference = legacyNativeReferenceForMode(request.Mode)
	}
	if request.Mode == WorkspaceWriteMode {
		approval = "acceptEdits"
		predicateReference = WorkspaceWriteReference()
	}
	effective, err := commonprovider.NativeEffectiveConfig(request, environment.WritableRoots, runtime.SHA256, approval)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	prepared := commonprovider.PreparedProfile{
		Plan: execution.Plan{
			Executable: cli.Path, Arguments: slices.Clone(arguments), Directory: request.CanonicalCwd,
			Environment: slices.Clone(environment.Values), Predicate: predicateReference, InputFiles: slices.Clone(inputs),
		},
		ObservedVersion: runtime.Version, Effective: effective, WritableRoots: slices.Clone(environment.WritableRoots),
		Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return NewIdentityObserver(request.RootID, request.TaskID, expected, record)
		},
	}
	if err = prepared.Validate(request); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return prepared, nil
}

func finalizeLegacyPreparedProfile(
	request task.TaskRecord,
	arguments []string,
	inputs []task.InputFile,
	cli commonprovider.CLIInfo,
	environment profileEnvironment,
	sources []task.PolicySourceDigest,
	definitionDigest string,
	predicateReference task.PredicateRef,
	data json.RawMessage,
	now time.Time,
) (commonprovider.PreparedProfile, error) {
	facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
	if decodeErr != nil || facts.Runtime == nil || len(facts.Native) == 0 || facts.Runtime.Executable != cli.Path || facts.Runtime.SHA256 != cli.SHA256 {
		return commonprovider.PreparedProfile{}, unsupportedNativeFacts()
	}
	effective, err := finalizePolicy(request, environment, sources, definitionDigest, facts.Native, now)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	prepared := commonprovider.PreparedProfile{
		Plan: execution.Plan{
			Executable: cli.Path, Arguments: slices.Clone(arguments), Directory: request.CanonicalCwd,
			Environment: slices.Clone(environment.Values), Predicate: predicateReference, InputFiles: slices.Clone(inputs),
		},
		ObservedVersion: facts.Runtime.Version, Effective: effective, WritableRoots: slices.Clone(environment.WritableRoots),
		Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return NewIdentityObserver(request.RootID, request.TaskID, expected, record)
		},
	}
	if err = prepared.Validate(request); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return prepared, nil
}

func finalizePortablePolicy(request task.TaskRecord, environment profileEnvironment, sources []task.PolicySourceDigest, now time.Time) (task.EffectiveConfig, error) {
	if now.IsZero() || request.BudgetNanos <= 0 {
		return task.EffectiveConfig{}, fmt.Errorf("%w: invalid runtime inspection boundary", ErrUnsupportedProfile)
	}
	if err := task.ValidateSHA256(environment.RuntimeSHA256); err != nil {
		return task.EffectiveConfig{}, fmt.Errorf("%w: runtime identity: %w", ErrUnsupportedProfile, err)
	}
	sources = slices.Clone(sources)
	slices.SortFunc(sources, func(a, b task.PolicySourceDigest) int {
		if order := cmp.Compare(a.Path, b.Path); order != 0 {
			return order
		}
		return cmp.Compare(a.Kind, b.Kind)
	})
	profileRevision, approval := ProfileRevision, "dontAsk"
	if request.Mode == WorkspaceWriteMode {
		profileRevision, approval = WorkspaceWriteProfileRevision, "acceptEdits"
	}
	effective := task.EffectiveConfig{Containment: request.Mode, Approval: approval, Policy: &task.PolicyDetails{
		ProfileRevision: profileRevision, RuntimeSHA256: environment.RuntimeSHA256,
		Workspace: request.CanonicalCwd, WritableRoots: slices.Clone(environment.WritableRoots), Sources: sources,
	}}
	encoded, err := task.MarshalCanonical(effective)
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	effective.Digest = task.ComputeSHA256(encoded)
	if err = task.ValidateEffectiveConfig(effective); err != nil {
		return task.EffectiveConfig{}, err
	}
	return effective, nil
}

func finalizePolicy(request task.TaskRecord, environment profileEnvironment, sources []task.PolicySourceDigest, definitionDigest string, data json.RawMessage, now time.Time) (task.EffectiveConfig, error) {
	if now.IsZero() || request.BudgetNanos <= 0 || task.ValidateSHA256(definitionDigest) != nil {
		return task.EffectiveConfig{}, unsupportedNativeFacts()
	}
	facts, err := decodePolicyFacts(data, now.Add(time.Duration(request.BudgetNanos)).Add(refreshMargin))
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	proof, err := task.MarshalCanonical(struct {
		Revision         string            `json:"revision"`
		DefinitionSHA256 string            `json:"definition_sha256"`
		Facts            nativePolicyFacts `json:"facts"`
	}{nativeInspectionRevision, definitionDigest, facts})
	if err != nil {
		return task.EffectiveConfig{}, unsupportedNativeFacts()
	}
	sources = append(slices.Clone(sources), task.PolicySourceDigest{
		Path: nativeCredentialHelper, Kind: "native-oauth-policy-proof", Present: true, SHA256: task.ComputeSHA256(proof),
	})
	slices.SortFunc(sources, func(a, b task.PolicySourceDigest) int {
		if order := cmp.Compare(a.Path, b.Path); order != 0 {
			return order
		}
		return cmp.Compare(a.Kind, b.Kind)
	})
	if err = task.ValidateSHA256(environment.RuntimeSHA256); err != nil {
		return task.EffectiveConfig{}, fmt.Errorf("%w: runtime identity: %w", ErrUnsupportedProfile, err)
	}
	profileRevision, approval := ProfileRevision, "dontAsk"
	if request.Mode == WorkspaceWriteMode {
		profileRevision, approval = WorkspaceWriteProfileRevision, "acceptEdits"
	}
	effective := task.EffectiveConfig{Containment: request.Mode, Approval: approval, Policy: &task.PolicyDetails{
		ProfileRevision: profileRevision, RuntimeSHA256: environment.RuntimeSHA256,
		Workspace: request.CanonicalCwd, WritableRoots: slices.Clone(environment.WritableRoots), Sources: sources,
	}}
	encoded, err := task.MarshalCanonical(effective)
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	effective.Digest = task.ComputeSHA256(encoded)
	if err = task.ValidateEffectiveConfig(effective); err != nil {
		return task.EffectiveConfig{}, err
	}
	return effective, nil
}
