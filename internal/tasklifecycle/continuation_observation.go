package tasklifecycle

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func ObserveTimeoutContinuation(root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog) (*ContinuationOffer, error) {
	records, err := ReadTimeoutRecords(td)
	if err != nil {
		return nil, err
	}
	return ObserveTimeoutContinuationWithRecords(root, td, req, meta, records, catalog)
}

func ObserveTimeoutContinuationWithRecords(root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog) (*ContinuationOffer, error) {
	runnerReleased, err := td.RunnerLeaseReleased()
	if err != nil {
		return nil, err
	}
	return ObserveTimeoutContinuationWithRunner(root, td, req, meta, records, catalog, runnerReleased)
}

func ObserveTimeoutContinuationWithRunner(root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog, runnerReleased bool) (*ContinuationOffer, error) {
	if !HasTerminatedBudgetStop(records) {
		return nil, nil
	}
	session, err := readProviderSession(td)
	if err != nil {
		return nil, err
	}
	runner := ""
	commandRunner := ""
	if meta != nil {
		runner = meta.RunnerExecutable
		ownership, ownershipErr := supervisorcore.RunnerOwnershipForTask(root, td, meta)
		if ownershipErr != nil {
			return nil, ownershipErr
		}
		commandRunner = TimeoutContinuationCommandRunner(meta, ownership)
	}
	launchStateRecorded := meta != nil && (meta.EnvironmentRecorded || len(meta.Environment) > 0)
	return TimeoutContinuation(req, records, catalog, session, runner, commandRunner, savedSupervisor(meta), runnerReleased, launchStateRecorded), nil
}

func observeTimeoutContinuationOnce(annotated *bool, observed *Observation, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog) error {
	if *annotated {
		return nil
	}
	offer, err := ObserveTimeoutContinuation(root, td, req, meta, catalog)
	if offer != nil {
		observed.Continuation = offer
	}
	if err == nil && continuationIsFinal(observed.Continuation) {
		*annotated = true
	}
	return err
}
