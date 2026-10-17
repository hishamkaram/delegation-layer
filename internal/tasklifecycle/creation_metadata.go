package tasklifecycle

import (
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func NewMeta(req task.TaskRecord, profile commonprovider.PreparedProfile, supervisor task.SupervisorRef, runner, ownership, publisher string) task.MetaRecord {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return task.MetaRecord{SchemaVersion: task.SchemaVersion, RootID: req.RootID, TaskID: req.TaskID, RequestedConfig: req.RequestedConfig, EffectiveConfig: profile.Effective, Containment: profile.Effective.Containment, Approval: profile.Effective.Approval, ProviderExecutable: profile.Plan.Executable, ProviderVersion: profile.ObservedVersion, RuntimePreflightSHA256: profile.RuntimePreflightSHA256, RunnerExecutable: runner, RunnerOwnership: ownership, PublisherBuild: publisher, PublisherVersion: publisher, Environment: cloneEnvironment(profile.Plan.Environment), EnvironmentRecorded: profile.Plan.Environment != nil, Predicate: profile.Plan.Predicate, InputFiles: profile.Plan.InputFiles, OutputArtifacts: profile.Plan.OutputArtifacts, OutputWriterContract: profile.Plan.OutputWriterContract, SupervisorConfig: supervisor, CreatedAt: now}
}

func cloneEnvironment(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}
