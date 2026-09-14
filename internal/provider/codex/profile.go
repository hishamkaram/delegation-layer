package codex

import (
	"os"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareCandidate performs finite local inspection only. Execution, capture,
// stopping and publication remain owned by the shared dispatcher and runner.
func PrepareCandidate(request task.TaskRecord) (commonprovider.PreparedProfile, error) {
	arguments, output, err := execArguments(request)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	executable, err := resolveExecutable()
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	environment, err := prepareEnvironment(os.Environ())
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	effective, err := effectivePolicy(request, environment, time.Now(), managedSources)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	prepared := commonprovider.PreparedProfile{
		Plan: execution.Plan{
			Executable: executable, Arguments: arguments, Directory: request.CanonicalCwd,
			Environment: environment.Values, Predicate: Reference(),
			OutputArtifacts:      []task.OutputArtifact{{Name: OutputName, ArgumentIndex: output}},
			OutputWriterContract: OutputWriterContract,
		},
		ObservedVersion: Version, Effective: effective, WritableRoots: slices.Clone(environment.WritableRoots),
		Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return NewIdentityObserver(request.TaskID, expected, record)
		},
	}
	if err = prepared.Validate(request); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return prepared, nil
}
