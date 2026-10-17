package app

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

const runnerReceiptPollInterval = tasklifecycle.RunnerReceiptPollInterval

func runTerminalEvent(parsed runnerArguments, deps Dependencies) error {
	return supervisorcore.RunTerminalEvent(parsed.Root, parsed.PueueID, parsed.ExitCode, deps.SupervisorOptions)
}

func runQueueMaintenance(parsed runnerArguments, deps Dependencies) error {
	return supervisorcore.RunQueueMaintenance(parsed.Root, deps.SupervisorOptions)
}

func reconcileRunner(ctx context.Context, root string, response *Response, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, supervisorOptions pueue.Options, initialSupervisorExecutable string) (*pueue.Client, pueue.Observation, error) {
	observed := tasklifecycle.Observation{}
	client, observation, err := tasklifecycle.ReconcileRunner(ctx, root, &observed, td, req, meta, observationSupervisorOptions(supervisorOptions, initialSupervisorExecutable))
	applyTaskObservation(response, observed, root)
	return client, observation, err
}

func resumeSupervisedRunner(ctx context.Context, response *Response, td *taskdir.TaskDir, supervisorOptions pueue.Options) (*pueue.Client, pueue.Observation, error) {
	observed := tasklifecycle.Observation{}
	client, observation, err := tasklifecycle.ResumeSupervisedRunner(ctx, &observed, td, supervisorOptions)
	applyTaskObservation(response, observed, "")
	return client, observation, err
}
