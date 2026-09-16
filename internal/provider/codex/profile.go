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
	arguments, output, err := execArguments(request)
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
	effective, err := effectivePolicy(request, environment, time.Now(), managedSources, cli.SHA256)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, environment.Values, RuntimeRequirements())
	if err != nil {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: runtime inspection: %w", ErrUnsupportedProfile, err)
	}
	return commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize: func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
			if decodeErr != nil || facts.Runtime == nil || facts.Native != nil {
				return commonprovider.PreparedProfile{}, fmt.Errorf("%w: invalid runtime inspection facts", ErrUnsupportedProfile)
			}
			runtime := *facts.Runtime
			if runtime.Executable != cli.Path || runtime.SHA256 != cli.SHA256 {
				return commonprovider.PreparedProfile{}, fmt.Errorf("%w: runtime executable changed during inspection", ErrUnsupportedProfile)
			}
			predicateReference := Reference()
			if request.Mode == WorkspaceWriteMode {
				predicateReference = WorkspaceWriteReference()
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
			if validateErr := prepared.Validate(request); validateErr != nil {
				return commonprovider.PreparedProfile{}, validateErr
			}
			return prepared, nil
		},
	}, nil
}
