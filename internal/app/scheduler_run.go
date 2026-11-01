package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	schedulerruntime "github.com/hishamkaram/delegation-layer/internal/scheduler/runtime"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// runScheduler owns the root/store/lease through the loop's joined shutdown.
// Startup opens existing storage only; schedule create owns initialization.
func runScheduler(ctx context.Context, a Arguments, deps Dependencies, stderr io.Writer) (result commandResult) {
	response := newResponse("scheduler")
	if ctx == nil {
		return failed(response, errors.New("scheduler requires a context"), 2)
	}
	if supervisorcore.RequestedRunnerOwnership(supervisorAdmissionSelection(a, deps)) != task.RunnerOwnershipManaged {
		return failed(response, errors.New("scheduler run requires Delegate's managed worker; omit --runner and invoke the matching colocated delegate/delegate-run build"), 2)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	store, err := openStoreContext(ctx, root, deps.storeDependencies(), false)
	if err != nil {
		return failed(response, schedulerStorageError(err), 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	response.RootID = store.RootID
	schedules, err := scheduler.Open(ctx, store)
	if err != nil {
		return failed(response, schedulerStorageError(err), 1)
	}
	var lease *scheduler.OwnerLease
	defer func() {
		mergeCommandClose(&result, schedules.Close)
		if lease != nil {
			mergeCommandClose(&result, lease.Close)
		}
	}()
	lease, err = scheduler.AcquireOwnerLease(ctx, store)
	if err != nil {
		return failed(response, schedulerStorageError(err), 1)
	}
	deps.SupervisorOptions.WaitForPendingClose = true
	deps = deps.withCapturedEnvironment()
	tasks := schedulerTasks{root: root, rootID: store.RootID, arguments: a, dependencies: deps}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var reportMu sync.Mutex
	var reportErr error
	err = schedulerruntime.Run(runCtx, schedulerruntime.Config{
		Store: schedules, Tasks: tasks,
		Report: func(event schedulerruntime.Event) {
			reportMu.Lock()
			defer reportMu.Unlock()
			if reportErr != nil {
				return
			}
			_, reportErr = fmt.Fprintf(stderr, "scheduler phase=%s schedule_id=%s task_id=%s code=%s\n", event.Phase, event.ScheduleID, event.TaskID, event.Code)
			if reportErr != nil {
				cancel()
			}
		},
	})
	err = errors.Join(err, reportErr)
	return failed(response, err, schedulerResultCode(err))
}

func schedulerStorageError(err error) error {
	if onlyNotExist(err) {
		return fmt.Errorf("scheduler storage is absent; create a schedule first: %w", err)
	}
	return err
}

func schedulerResultCode(err error) int {
	if err != nil {
		return 1
	}
	return 0
}
