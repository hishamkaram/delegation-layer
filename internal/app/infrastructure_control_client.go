package app

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// Stop retains its existing exact external endpoint checks. Private recovery
// uses the committed stop locator and independent routing; an unknown contact
// cannot become a local daemon Start or a repeated queue mutation.
func (d Dependencies) supervisorControlClientOpener() tasklifecycle.ControlClientOpener {
	routing := d.infrastructureRouting()
	return func(ctx context.Context, root string, saved task.SupervisorRef, source infrastructure.SavedBindingSource, options pueue.Options, lookup supervisor.ExecutableLookup) (*pueue.Client, error) {
		if !pueue.IsPrivateConfig(root, saved.ConfigPath) {
			return supervisor.NewControlClient(ctx, root, saved, options, lookup, openNativeSupervisorStore)
		}
		runtime := supervisor.ControlRuntime{Options: options, Lookup: lookup, Routing: routing, OpenStore: openNativeSupervisorStore}
		return runtime.OpenSaved(ctx, root, saved, source)
	}
}
