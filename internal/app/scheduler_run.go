package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/host"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/pulse"
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
	var hostLease *host.Lease
	var foreground *host.Foreground
	defer func() {
		mergeCommandClose(&result, func() error {
			return closeSchedulerRuntimeOwners(context.WithoutCancel(ctx), schedules, lease, hostLease, foreground)
		})
	}()
	if !a.schedulerResident {
		hostLease, err = host.AcquireHostLease(ctx, store)
		if err != nil {
			return failed(response, err, 1)
		}
		foreground, err = hostLease.AnnounceForeground(ctx, store)
		if err != nil {
			return failed(response, err, 1)
		}
	}
	lease, err = scheduler.AcquireOwnerLease(ctx, store)
	if err != nil {
		if a.schedulerResident && task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, task.ErrLockBusy) }) {
			return failed(response, err, host.ChildBusyExitCode)
		}
		return failed(response, schedulerStorageError(err), 1)
	}
	run := func(runCtx context.Context) error {
		return schedulerRuntime(runCtx, root, store.RootID, schedules, a, deps, stderr)
	}
	if a.schedulerResident {
		err = host.RunChild(ctx, host.ChildConfig{
			RootID: store.RootID,
			ReadFD: a.schedulerHostReadFD, WriteFD: a.schedulerHostWriteFD,
			BeforeReady: func(checkCtx context.Context) error { return checkSchedulerDefinitions(checkCtx, schedules) },
		}, run)
	} else {
		err = run(ctx)
	}
	return failed(response, err, schedulerResultCode(err))
}

func closeSchedulerRuntimeOwners(ctx context.Context, schedules contract.Store, execution *scheduler.OwnerLease, controller *host.Lease, foreground *host.Foreground) error {
	resultErr := schedules.Close()
	if execution != nil {
		closeErr := execution.Close()
		resultErr = errors.Join(resultErr, closeErr)
		if closeErr == nil && foreground != nil {
			resultErr = errors.Join(resultErr, foreground.Finish(ctx))
		}
	}
	if foreground != nil {
		resultErr = errors.Join(resultErr, foreground.Close())
	}
	if controller != nil {
		resultErr = errors.Join(resultErr, controller.Close())
	}
	return resultErr
}

func schedulerRuntime(ctx context.Context, root, rootID string, schedules contract.Store, a Arguments, deps Dependencies, stderr io.Writer) (resultErr error) {
	deps.SupervisorOptions.WaitForPendingClose = true
	deps = deps.withCapturedEnvironmentContext(ctx)
	wakes, err := pulse.New()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, wakes.Close()) }()
	wakes.Start()
	tasks := schedulerTasks{root: root, rootID: rootID, arguments: a, dependencies: deps, routing: deps.infrastructureRouting()}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var reportMu sync.Mutex
	var reportErr error
	err = schedulerruntime.Run(runCtx, schedulerruntime.Config{
		Store: schedules, Tasks: tasks, Waker: wakes, Evidence: tasks, Hooks: schedulerHooks{dependencies: deps},
		Report: func(event schedulerruntime.Event) {
			reportMu.Lock()
			defer reportMu.Unlock()
			if reportErr != nil {
				return
			}
			_, reportErr = fmt.Fprintf(stderr, "scheduler phase=%s schedule_id=%s task_id=%s code=%s stage=%s next_action=%s\n", event.Phase, event.ScheduleID, event.TaskID, event.Code, event.Stage, event.NextAction)
			if reportErr != nil {
				cancel()
			}
		},
	})
	err = errors.Join(err, reportErr)
	return schedulerRuntimeError(err, a.schedulerResident)
}

func schedulerRuntimeError(err error, resident bool) error {
	if resident && errors.Is(err, schedulerruntime.ErrTaskCleanupUnresolved) {
		return fmt.Errorf("%w: scheduler task cleanup remains unresolved: %w", host.ErrCleanupUnresolved, err)
	}
	if resident && (errors.Is(err, task.ErrUnsupportedSchema) || errors.Is(err, contract.ErrMigrationRequired) || errors.Is(err, task.ErrInvariantFault)) {
		return fmt.Errorf("%w: scheduler storage validation failed: %w", host.ErrIncompatible, err)
	}
	return err
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
