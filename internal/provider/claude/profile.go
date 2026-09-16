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

// PrepareCandidate resolves static inputs and describes native inspection.
// The shared app owns the effect and supplies nonsecret facts to Finalize.
func PrepareCandidate(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	arguments, inputs, err := printArguments(request)
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
	requirements := RuntimeRequirements()
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
			return finalizePreparedProfile(request, arguments, inputs, cli, environment, sources, definitionDigest, data, now)
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
) (commonprovider.PreparedProfile, error) {
	facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
	if decodeErr != nil || facts.Runtime == nil || len(facts.Native) == 0 || facts.Runtime.Executable != cli.Path || facts.Runtime.SHA256 != cli.SHA256 {
		return commonprovider.PreparedProfile{}, unsupportedNativeFacts()
	}
	effective, err := finalizePolicy(request, environment, sources, definitionDigest, facts.Native, now)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	predicateReference := ReferenceForMode(request.Mode)
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
