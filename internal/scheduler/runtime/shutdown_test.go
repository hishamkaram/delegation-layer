package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func nextWait(t *testing.T, clock *controlledClock) clockWait {
	t.Helper()
	for {
		wait := receive(t, clock.waits)
		if wait.ctx.Err() == nil {
			if wait.duration > time.Second {
				t.Fatalf("idle wait=%s", wait.duration)
			}
			return wait
		}
	}
}

func TestSchedulerRunShutdownJoinsHeldPendingControl(t *testing.T) {
	store, taskStore, lease := databaseWithOwner(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	clock := newClock(created.Add(30 * time.Second))
	started := make(chan DispatchRequest, 4)
	settle := make(chan struct{})
	ownerCtx := t.Context()
	var settled atomic.Bool
	tasks := blockedTasks(t.Context(), started)
	tasks.dispatch = func(request DispatchRequest) (TaskState, error) {
		started <- request
		select {
		case <-settle: // Owned control completion is unrelated to service stop.
		case <-ownerCtx.Done():
			return TaskState{}, ownerCtx.Err()
		}
		settled.Store(true)
		return absent(clock.Now()), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	type runResult struct {
		err     error
		settled bool
	}
	done := make(chan runResult, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		err := Run(ctx, Config{Store: store, Tasks: tasks, Clock: clock})
		done <- runResult{err: errors.Join(err, lease.Close()), settled: settled.Load()}
	}()
	t.Cleanup(func() {
		cancel()
		receive(t, finished)
	})
	wait := nextWait(t, clock)
	clock.set(created.Add(time.Minute))
	close(wait.wake)
	request := receive(t, started)
	wait = nextWait(t, clock)
	cancel()
	receive(t, wait.ctx.Done())
	assertOwnerHeld(t, taskStore)
	close(settle)
	result := receive(t, done)
	if result.err != nil || !result.settled {
		t.Fatalf("Run returned before control settlement: %+v", result)
	}
	// The test owns Store. Shutdown must preserve the armed row for recovery.
	stored, err := store.GetOccurrence(context.Background(), request.TaskID)
	if err != nil || stored.Release != nil {
		t.Fatalf("shutdown occurrence=%+v %v", stored, err)
	}
	if acquired, acquireErr := scheduler.AcquireOwnerLease(ownerCtx, taskStore); acquireErr != nil {
		t.Fatalf("caller could not reacquire after settled Run: %v", acquireErr)
	} else if closeErr := acquired.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestSchedulerRunJoinsObservationOnCancellation(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	clock := newClock(created.Add(90 * time.Second))
	observed := make(chan string, 1)
	settle := make(chan struct{})
	ownerCtx := t.Context()
	tasks := blockedTasks(t.Context(), make(chan DispatchRequest, 4))
	var settled atomic.Bool
	tasks.observe = func(taskID string) (TaskState, error) {
		observed <- taskID
		select {
		case <-settle:
		case <-ownerCtx.Done():
			return TaskState{}, ownerCtx.Err()
		}
		settled.Store(true)
		return pending(clock.Now(), task.AdmissionUnknown), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	type runResult struct {
		err     error
		settled bool
	}
	done := make(chan runResult, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		err := Run(ctx, Config{Store: store, Tasks: tasks, Clock: clock})
		done <- runResult{err: err, settled: settled.Load()}
	}()
	t.Cleanup(func() { cancel(); receive(t, finished) })
	if got := receive(t, observed); got != prior.TaskID {
		t.Fatalf("observed %s", got)
	}
	wait := nextWait(t, clock)
	cancel()
	receive(t, wait.ctx.Done())
	close(settle)
	result := receive(t, done)
	if result.err != nil || !result.settled {
		t.Fatalf("unjoined observation: %+v", result)
	}
}

func TestSchedulerNativeClockCancellationAndRequiredOwners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (nativeClock{}).Wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("native wait: %v", err)
	}
	if err := Run(ctx, Config{}); err == nil {
		t.Fatal("missing owners accepted")
	}
	store := database(t)
	if err := Run(ctx, Config{Store: store, Tasks: blockedTasks(t.Context(), make(chan DispatchRequest, 4))}); err != nil {
		t.Fatalf("canceled Run: %v", err)
	}
	if _, err := store.ListSchedules(context.Background(), contract.SchedulePage{Limit: 100}); err != nil {
		t.Fatalf("Run closed caller-owned Store: %v", err)
	}
}

func TestSchedulerRunPreservesRootIdentityFaultOnShutdown(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	clock := newClock(created.Add(30 * time.Second))
	started := make(chan DispatchRequest, 1)
	settle := make(chan struct{})
	ownerCtx := t.Context()
	tasks := blockedTasks(t.Context(), started)
	tasks.dispatch = func(request DispatchRequest) (TaskState, error) {
		started <- request
		select {
		case <-settle:
		case <-ownerCtx.Done():
			return TaskState{}, ownerCtx.Err()
		}
		return TaskState{}, errors.Join(ErrRootIdentity, task.ErrIdentityMismatch)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() { defer close(finished); done <- Run(ctx, Config{Store: store, Tasks: tasks, Clock: clock}) }()
	t.Cleanup(func() { cancel(); receive(t, finished) })
	wait := nextWait(t, clock)
	clock.set(created.Add(time.Minute))
	close(wait.wake)
	receive(t, started)
	wait = nextWait(t, clock)
	cancel()
	receive(t, wait.ctx.Done())
	close(settle)
	if err := receive(t, done); !errors.Is(err, ErrRootIdentity) || !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("shutdown hid root identity fault: %v", err)
	}
}
