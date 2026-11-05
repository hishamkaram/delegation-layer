package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSchedulerCleanupUnresolvedJoinsAllWorkersAndErrorGraphs(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "fault-first", true: "cancel-first"}[cancelFirst], func(t *testing.T) { exerciseSchedulerCleanupUnresolved(t, cancelFirst) })
	}
}

func exerciseSchedulerCleanupUnresolved(t *testing.T, cancelFirst bool) {
	t.Helper()
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	create(t, store, 2, created)
	clock := newClock(created.Add(30 * time.Second))
	waker := newWaker()
	started := make(chan DispatchRequest, 2)
	reported := make(chan Event, 2)
	firstRelease, secondRelease := make(chan struct{}), make(chan struct{})
	var firstOnce, secondOnce sync.Once
	releaseFirst := func() { firstOnce.Do(func() { close(firstRelease) }) }
	releaseSecond := func() { secondOnce.Do(func() { close(secondRelease) }) }
	first, second := errors.New("first retained owner"), errors.New("second retained owner")
	tasks := taskFuncs{observe: func(string) (TaskState, error) { return pending(clock.Now(), task.AdmissionUnknown), nil }, dispatch: func(request DispatchRequest) (TaskState, error) {
		started <- request
		if request.Definition.ScheduleID == id(1) {
			<-firstRelease
			return TaskState{}, fmt.Errorf("first: %w", errors.Join(ErrTaskCleanupUnresolved, first, task.ErrInvariantFault))
		}
		<-secondRelease
		return TaskState{}, fmt.Errorf("second: %w", errors.Join(ErrTaskCleanupUnresolved, second))
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() { cancel(); releaseFirst(); releaseSecond(); receive(t, finished) })
	go func() {
		defer close(finished)
		done <- Run(ctx, Config{Store: store, Tasks: tasks, Clock: clock, Waker: waker, Report: func(event Event) {
			if event.Code == "task_error" {
				reported <- event
			}
		}})
	}()
	wait := nextWait(t, waker)
	clock.set(created.Add(time.Minute))
	close(wait.wake)
	receive(t, started)
	receive(t, started)
	if cancelFirst {
		wait = nextWait(t, waker)
		cancel()
		receive(t, wait.ctx.Done())
	}
	releaseFirst()
	if !cancelFirst {
		receive(t, reported)
	}
	select {
	case err := <-done:
		t.Fatalf("Run discarded the second invocation: %v", err)
	default:
	}
	releaseSecond()
	if !cancelFirst {
		receive(t, reported)
		cancel()
	}
	err := receive(t, done)
	for _, cause := range []error{ErrTaskCleanupUnresolved, first, second, task.ErrInvariantFault} {
		if !errors.Is(err, cause) {
			t.Fatalf("Run lost marked graph cause %v: %v", cause, err)
		}
	}
	for _, number := range []int{1, 2} {
		for _, entry := range history(t, store, id(number)) {
			if occurrence, ok := entry.Occurrence(); ok && occurrence.Release != nil {
				t.Fatal("cleanup fault fabricated task release")
			}
		}
	}
}

func TestSchedulerTaskCleanupMarkerPrecedesCancellationAndState(t *testing.T) {
	first := errors.New("retained descriptor")
	l, ctx := testLoop(t, Config{Clock: newClock(instant("2026-01-01T00:00:00Z"))})
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	occurrence := contract.Occurrence{TaskID: id(3), ScheduleID: id(1)}
	l.admissions = 1
	l.live[occurrence.TaskID] = true
	err := l.finish(canceled, taskResult{occurrence: occurrence, err: errors.Join(ErrTaskCleanupUnresolved, first), dispatch: true})
	if !errors.Is(err, first) || !errors.Is(err, ErrTaskCleanupUnresolved) {
		t.Fatalf("marker replaced by cancellation/state: %v", err)
	}
}
