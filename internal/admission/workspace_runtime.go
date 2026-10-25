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
	options := runtime.controlOptions()
	client, err := supervisorcore.PreparePrivateAdmissionSupervisorContext(ctx, runtime.configuration.Lookup(), store, options)
	return runner, client, err
}

func (runtime workspaceRuntime) OpenSaved(ctx context.Context, root string, saved task.SupervisorRef) (*pueue.Client, error) {
	options := runtime.controlOptions()
	return supervisorcore.NewClient(ctx, root, saved, options, true, runtime.configuration.Lookup(), runtime.configuration.OpenStore)
}

// controlOptions binds supervisor control to the runtime's selected scope. An
// explicit control environment, including an empty one, is kept unchanged.
func (runtime workspaceRuntime) controlOptions() pueue.Options {
	return supervisorcore.OptionsForEnvironment(runtime.configuration.Control, runtime.configuration.Environment)
}
