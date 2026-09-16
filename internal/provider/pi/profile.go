package pi

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

// PrepareCandidate performs static command construction and describes the
// shared supervised runtime capability probe. The finalizer consumes only the
// nonsecret facts produced by that probe.
func PrepareCandidate(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	arguments, err := jsonArguments(request)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	environment, err := prepareEnvironment(os.Environ())
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if startupErr := validateStartupMigrations(request.CanonicalCwd, environment.AgentDir, environment.SessionDir); startupErr != nil {
		return commonprovider.ProfileCandidate{}, startupErr
	}
	arguments = append(arguments, "--session-dir", environment.SessionDir)
	cli, err := resolveExecutable()
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	sources, err := inspectPolicySources(environment.AgentDir, request.CanonicalCwd)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	effective, err := effectivePolicy(request, environment, cli.SHA256, sources)
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
			prepared := commonprovider.PreparedProfile{
				Plan: execution.Plan{
					Executable: cli.Path, Arguments: slices.Clone(arguments), Directory: request.CanonicalCwd,
					Environment: slices.Clone(environment.Values), Predicate: ReferenceForMode(request.Mode),
				},
				ObservedVersion: runtime.Version,
				Effective:       task.CloneEffectiveConfig(effective),
				WritableRoots:   slices.Clone(environment.WritableRoots),
				Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
					return NewIdentityObserver(expected, record)
				},
			}
			if validateErr := prepared.Validate(request); validateErr != nil {
				return commonprovider.PreparedProfile{}, validateErr
			}
			return prepared, nil
		},
	}, nil
}
