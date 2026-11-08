package admission

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func (runtime workspaceRuntime) ManagedRunner() (string, error) {
	return installation.ResolveRunner("", runtime.configuration.RunnerExecutable)
}

func (runtime workspaceRuntime) PrepareManaged(ctx context.Context, store *taskdir.Store, runner string) (string, *pueue.Client, error) {
	configuration := runtime.configuration
	prepared, err := supervisorcore.PreparePrivateAdmissionExecutablesContext(ctx, supervisorcore.AdmissionSelection{}, configuration, store.Root, runner)
	runner = prepared.Executable
	err = WithStage(prepared.FailureStage, err)
	if err != nil {
		return "", nil, err
	}
	options := runtime.controlOptions()
	client, err := (supervisorcore.ControlRuntime{Options: options, Lookup: configuration.Lookup(), Routing: configuration.Routing}).PreparePrivateAdmission(ctx, store)
	return runner, client, err
}

func (runtime workspaceRuntime) OpenSaved(ctx context.Context, store *taskdir.Store, saved task.SupervisorRef, source infrastructure.SavedBindingSource) (*pueue.Client, error) {
	options := runtime.controlOptions()
	return (supervisorcore.ControlRuntime{Options: options, Lookup: runtime.configuration.Lookup(), Routing: runtime.configuration.Routing}).RecoverSaved(ctx, store, saved, source)
}

// controlOptions binds supervisor control to the runtime's selected scope. An
// explicit control environment, including an empty one, is kept unchanged.
func (runtime workspaceRuntime) controlOptions() pueue.Options {
	return supervisorcore.OptionsForEnvironment(runtime.configuration.Control, runtime.configuration.Environment)
}
