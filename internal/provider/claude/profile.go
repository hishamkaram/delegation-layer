package claude

import (
	"cmp"
	"encoding/json"
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
	executable, err := resolveExecutable()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	environment, err := prepareEnvironment(os.Environ())
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	sources, err := resolvePolicySources(request, environment)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if err = inspectLegacyAPIKeyAbsence(environment); err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	definition, err := nativeInspection(environment)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	_, definitionDigest, err := definition.Snapshot()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize: func(data json.RawMessage, now time.Time) (commonprovider.PreparedProfile, error) {
			effective, finalErr := finalizePolicy(request, environment, sources, definitionDigest, data, now)
			if finalErr != nil {
				return commonprovider.PreparedProfile{}, finalErr
			}
			prepared := commonprovider.PreparedProfile{
				Plan:            execution.Plan{Executable: executable, Arguments: slices.Clone(arguments), Directory: request.CanonicalCwd, Environment: slices.Clone(environment.Values), Predicate: Reference(), InputFiles: slices.Clone(inputs)},
				ObservedVersion: Version, Effective: effective, WritableRoots: slices.Clone(environment.WritableRoots),
				Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
					return NewIdentityObserver(request.RootID, request.TaskID, expected, record)
				},
			}
			if finalErr = prepared.Validate(request); finalErr != nil {
				return commonprovider.PreparedProfile{}, finalErr
			}
			return prepared, nil
		},
	}, nil
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
	effective := task.EffectiveConfig{Containment: Mode, Approval: "dontAsk", Policy: &task.PolicyDetails{
		ProfileRevision: ProfileRevision, RuntimeSHA256: inspectedRuntimeSHA256,
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
