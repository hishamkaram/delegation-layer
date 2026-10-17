package app

import (
	"context"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func reconcileStatusWithClient(ctx context.Context, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string, client *pueue.Client, response *Response) (observation pueue.Observation, resultErr error) {
	observed := tasklifecycle.Observation{}
	observation, resultErr = tasklifecycle.ReconcileStatusWithClient(ctx, root, td, req, meta, spec, metaHash, client, &observed)
	applyTaskObservation(response, observed, root)
	return observation, resultErr
}

func updateStatusStopsContext(ctx context.Context, td *taskdir.TaskDir, response *Response, observation pueue.Observation) error {
	observed := tasklifecycle.Observation{}
	err := tasklifecycle.UpdateStatusStops(ctx, td, &observed, observation)
	applyTaskObservation(response, observed, "")
	return err
}

func applyTimeoutContinuationFromTask(response *Response, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog) error {
	offer, err := tasklifecycle.ObserveTimeoutContinuation(root, td, req, meta, catalog)
	applyContinuationOffer(response, root, offer)
	return err
}

func applyTimeoutContinuationWithRecords(response *Response, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog) error {
	offer, err := tasklifecycle.ObserveTimeoutContinuationWithRecords(root, td, req, meta, records, catalog)
	applyContinuationOffer(response, root, offer)
	return err
}

func applyTimeoutContinuationWithRunnerStateAndCommandRunner(response *Response, root string, req *task.TaskRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog, session *task.ProviderRefRecord, runner, commandRunner string, supervisor *task.SupervisorRef, runnerReleased, launchStateRecorded bool) {
	if response == nil {
		return
	}
	applyContinuationOffer(response, root, tasklifecycle.TimeoutContinuation(req, records, catalog, session, runner, commandRunner, supervisor, runnerReleased, launchStateRecorded))
}

func fillInteraction(response *Response, td *taskdir.TaskDir) error {
	state, err := tasklifecycle.ReadInteraction(td)
	if state != nil {
		response.Interaction = state
	}
	return err
}

func observationSupervisorOptions(options pueue.Options, initialExecutable string) tasklifecycle.SupervisorOptions {
	return tasklifecycle.SupervisorOptions{Options: options, InitialExecutable: initialExecutable, OpenStore: openNativeSupervisorStore}
}
