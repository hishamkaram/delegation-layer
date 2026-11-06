package admission

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func InspectProvider(options InspectionOptions, store *taskdir.Store, req task.TaskRecord, candidate commonprovider.ProfileCandidate, supervisor *pueue.Client, inspectionGroupReady bool) (facts json.RawMessage, deadline time.Time, resultErr error) {
	return InspectProviderContext(context.Background(), options, store, req, candidate, supervisor, inspectionGroupReady)
}

func InspectProviderContext(parent context.Context, options InspectionOptions, store *taskdir.Store, req task.TaskRecord, candidate commonprovider.ProfileCandidate, supervisor *pueue.Client, inspectionGroupReady bool) (facts json.RawMessage, deadline time.Time, resultErr error) {
	if parent == nil {
		return nil, time.Time{}, context.Canceled
	}
	if err := parent.Err(); err != nil {
		return nil, time.Time{}, err
	}
	runner, inspectionSupervisor, binding, err := prepareInspectionBinding(options, candidate, supervisor)
	if err != nil {
		return nil, time.Time{}, err
	}
	binding, err = inspectionReplayBinding(parent, store, req, candidate, binding)
	if err != nil {
		return nil, time.Time{}, err
	}
	inspectionTimeout := inspection.TimeoutForRevision(binding.DefinitionRevision)
	concurrent := ConcurrentInspection(store.Root, binding)
	if !concurrent && !inspectionGroupReady {
		err = ensureInspectionGroup(parent, options, store, inspectionSupervisor, inspectionTimeout)
	}
	if err != nil {
		return nil, time.Time{}, AnnotateMissingInspectionStage("ensuring inspection group", err)
	}

	openedAt := time.Now()
	setupContext, cancelSetup := context.WithDeadline(parent, openedAt.Add(inspectionTimeout))
	defer cancelSetup()
	operation, err := OpenInspectionOperationContext(setupContext, store, req, binding, openedAt)
	if err != nil {
		return nil, time.Time{}, AnnotateMissingInspectionStage("opening inspection operation", err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	// Replays retain their saved group, including legacy runtime probes. A
	// current capability marker must never rewrite an existing request.
	if concurrent && operation.Request().Group != "default" && !inspectionGroupReady {
		err = ensureInspectionGroup(parent, options, store, inspectionSupervisor, inspectionTimeout)
	}
	if err != nil {
		return nil, operation.Deadline(), AnnotateMissingInspectionStage("ensuring saved inspection group", err)
	}
	facts, deadline, err = RunInspectionContext(parent, operation, inspectionSupervisor, runner, store.Root)
	if err != nil {
		err = WithInspectionFailureContext(err, req.TaskID, deadline)
	}
	return facts, deadline, err
}

func prepareInspectionBinding(options InspectionOptions, candidate commonprovider.ProfileCandidate, supervisor *pueue.Client) (string, *pueue.Client, inspection.Binding, error) {
	runner, err := supervisorcore.ResolveRunner(options.Runner, options.DefaultRunner)
	if err != nil {
		return "", nil, inspection.Binding{}, AnnotateMissingInspectionStage("resolving worker", err)
	}
	definition, definitionSHA, err := candidate.Inspection.Snapshot()
	if err != nil {
		return "", nil, inspection.Binding{}, AnnotateMissingInspectionStage("snapshotting definition", err)
	}
	capabilitySHA := ""
	if runtimeOnlyInspectionDefinition(definition) {
		capabilitySHA, err = commonprovider.RuntimeCapabilityDigest(definition)
		if err != nil {
			return "", nil, inspection.Binding{}, AnnotateMissingInspectionStage("snapshotting capability", err)
		}
	}
	inspectionTimeout := inspection.TimeoutForRevision(definition.Revision)
	inspectionSupervisor, err := supervisor.WithObservationTimeout(inspectionTimeout)
	if err != nil {
		return "", nil, inspection.Binding{}, AnnotateMissingInspectionStage("preparing inspection supervisor", err)
	}
	binding := installedInspectionBinding(options, definition, definitionSHA, capabilitySHA, runner, supervisor.Binding())
	return runner, inspectionSupervisor, binding, nil
}

func installedInspectionBinding(options InspectionOptions, definition commonprovider.InspectionDefinition, definitionSHA, capabilitySHA, runner string, supervisor task.SupervisorRef) inspection.Binding {
	return inspection.Binding{
		DefinitionRevision: definition.Revision, DefinitionSHA256: definitionSHA, CapabilitySHA256: capabilitySHA,
		HelperExecutable: definition.Executable, WorkerExecutable: runner,
		ExecutableIdentity: task.ExecutableIdentityInstalledPathV1,
		RunnerOwnership:    options.RunnerOwnership,
		WorkerExitProof:    inspection.WorkerExitProofGuardianWaitV1,
		Environment:        append([]string(nil), definition.Environment...), EnvironmentRecorded: true,
		Supervisor: supervisor,
	}
}

func ensureInspectionGroup(ctx context.Context, options InspectionOptions, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration) error {
	if options.PreserveGeneration {
		return supervisorcore.EnsureInspectionGroupWithoutMigrationContext(ctx, store, supervisor, timeout)
	}
	return supervisorcore.EnsureInspectionGroupWithMigrationContext(ctx, store, supervisor, timeout, false)
}
