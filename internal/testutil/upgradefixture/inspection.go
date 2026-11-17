package main

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// openFixtureInspection uses the same runtime definition and digest constructors
// as production admission. It creates no worker and requires no native backend.
func openFixtureInspection(ctx context.Context, store *taskdir.Store, workspace, id, helper, worker string, supervisor task.SupervisorRef) (*inspection.Operation, error) {
	brief := []byte("Owned upgrade-maintenance fixture")
	request := task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id,
		Provider: "pi:json", Mode: "read-only", CanonicalCwd: workspace,
		RequestedConfig: task.TaskConfig{Permission: "read-only", Budget: "1m0s"}, BudgetNanos: int64(time.Minute),
		BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)),
	}
	definition, err := provider.NewRuntimeInspectionDefinition(provider.CLIInfo{Path: worker}, workspace, []string{}, provider.RuntimeCapability{HelpArgs: []string{"1"}})
	if err != nil {
		return nil, err
	}
	_, definitionDigest, err := definition.Snapshot()
	if err != nil {
		return nil, err
	}
	capabilityDigest, err := provider.RuntimeCapabilityDigest(definition)
	if err != nil {
		return nil, err
	}
	return inspection.OpenConcurrentOperationContext(ctx, store, request, inspection.Binding{
		DefinitionRevision: definition.Revision, DefinitionSHA256: definitionDigest, CapabilitySHA256: capabilityDigest,
		HelperExecutable: helper, WorkerExecutable: worker, ExecutableIdentity: task.ExecutableIdentityInstalledPathV1,
		RunnerOwnership: task.RunnerOwnershipManaged, WorkerExitProof: inspection.WorkerExitProofGuardianWaitV1,
		Environment: definition.Environment, EnvironmentRecorded: true, Supervisor: supervisor,
	}, time.Now())
}
