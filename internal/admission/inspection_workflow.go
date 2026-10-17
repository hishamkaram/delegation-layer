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
	runner, inspectionSupervisor, binding, err := prepareInspectionBinding(options, candidate, supervisor)
	if err != nil {
		return nil, time.Time{}, err
	}
	inspectionTimeout := inspection.TimeoutForRevision(binding.DefinitionRevision)
	concurrent := ConcurrentInspection(store.Root, binding)
	if !concurrent && !inspectionGroupReady {
		err = ensureInspectionGroup(options, store, inspectionSupervisor, inspectionTimeout)
	}
	if err != nil {
		return nil, time.Time{}, AnnotateMissingInspectionStage("ensuring inspection group", err)
	}

	openedAt := time.Now()
	setupContext, cancelSetup := context.WithDeadline(context.Background(), openedAt.Add(inspectionTimeout))
	defer cancelSetup()
	operation, err := OpenInspectionOperationContext(setupContext, store, req, binding, openedAt)
	if err != nil {
		return nil, time.Time{}, AnnotateMissingInspectionStage("opening inspection operation", err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	// Replays retain their saved group, including legacy runtime probes. A
	// current capability marker must never rewrite an existing request.
	if concurrent && operation.Request().Group != "default" && !inspectionGroupReady {
		err = ensureInspectionGroup(options, store, inspectionSupervisor, inspectionTimeout)
	}
	if err != nil {
		return nil, operation.Deadline(), AnnotateMissingInspectionStage("ensuring saved inspection group", err)
	}
	facts, deadline, err = RunInspection(operation, inspectionSupervisor, runner, store.Root)
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
	workerSHA, err := commonprovider.FingerprintExecutable(runner)
	if err != nil {
		return "", nil, inspection.Binding{}, AnnotateMissingInspectionStage("fingerprinting worker", err)
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
	binding := inspection.Binding{
		DefinitionRevision: definition.Revision, DefinitionSHA256: definitionSHA, CapabilitySHA256: capabilitySHA,
		HelperExecutable: definition.Executable, HelperSHA256: definition.ExecutableSHA256,
		WorkerExecutable: runner, WorkerSHA256: workerSHA,
		RunnerOwnership: options.RunnerOwnership,
		WorkerExitProof: inspection.WorkerExitProofGuardianWaitV1,
		Environment:     append([]string(nil), definition.Environment...), EnvironmentRecorded: true,
		Supervisor: supervisor.Binding(),
	}
	return runner, inspectionSupervisor, binding, nil
}

func ensureInspectionGroup(options InspectionOptions, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration) error {
	if options.PreserveGeneration {
		return supervisorcore.EnsureInspectionGroupWithoutMigration(store, supervisor, timeout)
	}
	return supervisorcore.EnsureInspectionGroup(store, supervisor, timeout)
}
