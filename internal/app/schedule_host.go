package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/host"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// inspectScheduleHost opens only existing metadata and the authenticated host
// endpoint. It never probes H or initializes scheduler/supervisor resources.
func inspectScheduleHost(ctx context.Context, a Arguments, deps Dependencies) (observed SchedulerResponse, err error) {
	observed.State = "no_host_observed"
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return observed, err
	}
	self, err := schedulerExecutable()
	if err != nil {
		return observed, err
	}
	observed.ServeCommand = []string{self.Path, "--root", root, "scheduler", "serve"}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.storeDependencies().registry())
	if onlyNotExist(err) {
		return observed, nil
	}
	if err != nil {
		return observed, err
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	status, err := host.Observe(ctx, host.Query{Store: reader, BuildID: self.SHA256})
	command := observed.ServeCommand
	observed = projectSchedulerStatus(status)
	observed.ServeCommand = command
	if err != nil {
		return observed, err
	}
	if status.State != "no_host_observed" && status.State != "stopped" && status.BuildID != "" {
		err = host.CheckCompatible(status, self.SHA256)
	}
	return observed, err
}

func checkScheduleHostBeforeMutation(a Arguments, deps Dependencies) error {
	ctx, cancel := context.WithTimeout(context.Background(), scheduleOperationTimeout)
	defer cancel()
	_, err := inspectScheduleHost(ctx, a, deps)
	return err
}

func finishScheduleHost(a Arguments, deps Dependencies, result commandResult) commandResult {
	if result.code != 0 || result.err != nil || result.response.Schedule == nil {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), scheduleOperationTimeout)
	defer cancel()
	observed, err := inspectScheduleHost(ctx, a, deps)
	return savedScheduleHostResult(result, observed, err)
}
