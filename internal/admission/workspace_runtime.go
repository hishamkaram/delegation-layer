package admission

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func (runtime workspaceRuntime) ManagedRunner() (string, error) {
	return supervisorcore.ResolveRunner("", runtime.configuration.RunnerExecutable)
}

func (runtime workspaceRuntime) PrepareManaged(ctx context.Context, store *taskdir.Store, runner string) (string, *pueue.Client, error) {
	configuration := runtime.configuration
	prepared, err := supervisorcore.PreparePrivateAdmissionExecutablesContext(ctx, supervisorcore.AdmissionSelection{}, configuration, store.Root, runner, task.RunnerOwnershipManaged)
	runner = prepared.Executable
	err = WithStage(prepared.FailureStage, err)
	if err != nil {
		return "", nil, err
	}
	options := supervisorcore.OptionsForCurrentEnvironment(runtime.configuration.Control)
	client, err := supervisorcore.PreparePrivateAdmissionSupervisorContext(ctx, runtime.configuration.InitialExecutable, store, options)
	return runner, client, err
}

func (runtime workspaceRuntime) OpenSaved(ctx context.Context, root string, saved task.SupervisorRef) (*pueue.Client, error) {
	options := supervisorcore.OptionsForCurrentEnvironment(runtime.configuration.Control)
	return supervisorcore.NewClient(ctx, root, saved, options, true, runtime.configuration.InitialExecutable, runtime.configuration.OpenStore)
}
