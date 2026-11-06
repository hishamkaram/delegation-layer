package tasklifecycle

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func (options SupervisorOptions) savedClient(ctx context.Context, root string, saved task.SupervisorRef, source infrastructure.SavedBindingSource) (*pueue.Client, error) {
	runtime := supervisorcore.ControlRuntime{Options: options.Options, Lookup: options.Lookup, Routing: options.Routing, OpenStore: options.OpenStore}
	if options.AdmissionStore != nil {
		return runtime.RecoverSaved(ctx, options.AdmissionStore, saved, source)
	}
	return runtime.OpenSaved(ctx, root, saved, source)
}

func (options SupervisorOptions) observeSubmittedSupervisor(ctx context.Context, store *taskdir.Store, submission *task.SubmitRecord, meta *task.MetaRecord, runner string) (supervisorcore.TaskSupervisorObservation, error) {
	source, err := supervisorcore.NewSavedBindingSource(submission.RootID, infrastructure.SavedBindingTaskSubmission, submission.TaskID, "", submission.Supervisor)
	if err != nil {
		return supervisorcore.TaskSupervisorObservation{}, err
	}
	return supervisorcore.ObserveTaskSupervisorWithOptions(ctx, store, submission.Supervisor, meta, supervisorcore.TaskSupervisorObservationOptions{Control: options.Options, Lookup: options.Lookup, RunnerExecutable: runner, OpenStore: options.OpenStore, Routing: options.Routing, Source: source})
}
