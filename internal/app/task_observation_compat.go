package app

import (
	"github.com/hishamkaram/delegation-layer/internal/pueue"

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

func observationSupervisorOptions(options pueue.Options, initialExecutable string) tasklifecycle.SupervisorOptions {
	return tasklifecycle.SupervisorOptions{Options: options, InitialExecutable: initialExecutable, OpenStore: openNativeSupervisorStore}
}
