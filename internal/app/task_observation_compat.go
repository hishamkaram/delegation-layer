package app

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func fillInteraction(response *Response, td *taskdir.TaskDir) error {
	state, err := tasklifecycle.ReadInteraction(td)
	if state != nil {
		response.Interaction = state
	}
	return err
}

func observationSupervisorOptions(options pueue.Options, lookup installation.ExecutableLookup, routing supervisorcore.OwnerRouting) tasklifecycle.SupervisorOptions {
	return tasklifecycle.SupervisorOptions{Options: options, Lookup: lookup, Routing: routing, OpenStore: openNativeSupervisorStore}
}
