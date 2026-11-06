package app

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/host"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

func runSchedulerCommand(ctx context.Context, a Arguments, deps Dependencies, stderr io.Writer) commandResult {
	switch a.SchedulerAction {
	case "serve":
		return serveScheduler(ctx, a, deps)
	case "status", "stop":
		return controlScheduler(ctx, a, deps)
	default:
		return runScheduler(ctx, a, deps, stderr)
	}
}

func schedulerExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return verifiedexec.LocatePath(path)
}

func serveScheduler(ctx context.Context, a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("scheduler")
	if err := lifetime.CheckIndependentHost(ctx, deps.Environment); err != nil {
		code := 1
		if errors.Is(err, lifetime.ErrHostContained) {
			code = 2
		}
		return failed(response, err, code)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	if a.Runner != "" {
		return failed(response, errors.New("scheduler serve requires the colocated managed delegate-run; omit --runner"), 2)
	}
	if _, err = supervisorcore.ResolveRunner("", deps.RunnerExecutable); err != nil {
		return failed(response, err, 1)
	}
	self, err := schedulerExecutable()
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStoreContext(ctx, root, deps.storeDependencies(), true)
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	response.RootID = store.RootID
	schedules, err := openOrInitializeSchedules(ctx, store)
	if err != nil {
		return failed(response, err, 1)
	}
	if err = schedules.Close(); err != nil {
		return failed(response, err, 1)
	}
	args := []string{"--root", root, "scheduler", "run"}
	if a.PueueConfig != "" {
		args = append(args, "--pueue-config", a.PueueConfig)
	}
	err = host.Serve(ctx, host.Config{
		Store: store, Executable: self, Args: args,
		Environment: deps.Environment, Directory: root,
	})
	if err == nil {
		response.Scheduler = &SchedulerResponse{State: "stopped"}
	}
	return failed(response, err, schedulerHostResultCode(err))
}

func schedulerHostResultCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, host.ErrPending) && task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, host.ErrPending) || errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, task.ErrLockBusy)
	}) {
		return 3
	}
	return 1
}

func projectSchedulerStatus(observed host.Status) SchedulerResponse {
	return SchedulerResponse{
		State: observed.State, Epoch: observed.Epoch, Attempt: observed.Attempt,
		Ready: observed.Ready, Fault: observed.Fault, NextRetryAt: observed.NextRetryAt, LastError: observed.LastError,
	}
}

func controlScheduler(ctx context.Context, a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("scheduler")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, root, deps.storeDependencies().registry())
	if onlyNotExist(err) {
		response.Scheduler = &SchedulerResponse{State: "no_host_observed", NextAction: "start_required"}
		return commandResult{response: response}
	}
	if err != nil {
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, reader.Close) }()
	response.RootID = reader.RootID()
	query := host.Query{Store: reader}
	if a.SchedulerAction == "stop" {
		stopped, stopErr := host.Stop(ctx, query)
		observed := projectSchedulerStatus(stopped.Status)
		if stopped.Accepted || observed.State == "no_host_observed" || observed.State == "foreground" {
			observed.StopAccepted = &stopped.Accepted
		}
		if observed.State == "foreground" {
			observed.NextAction = "stop_foreground_runtime"
		}
		response.Scheduler = &observed
		code := schedulerHostResultCode(stopErr)
		if code == 3 {
			response.Pending = &PendingResponse{Kind: "scheduler"}
			return commandResult{response: response, code: code}
		}
		return failed(response, stopErr, code)
	}
	observed, err := host.Observe(ctx, query)
	projection := projectSchedulerStatus(observed)
	response.Scheduler = &projection
	return failed(response, err, schedulerHostResultCode(err))
}
