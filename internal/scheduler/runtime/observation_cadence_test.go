package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSchedulerFreshDueObservationOverridesEarlierIdleVisit(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	prior := claimPrior(t, store, 1, 91, created, created.Add(time.Minute))
	clock := newClock(created.Add(2*time.Minute - 500*time.Millisecond))
	observed := make(chan string, 2)
	started := make(chan DispatchRequest, 1)
	calls := 0
	tasks := blockedTasks(t.Context(), started)
	tasks.observe = func(taskID string) (TaskState, error) {
		observed <- taskID
		calls++
		if calls == 1 {
			return pending(clock.Now(), task.AdmissionAdmitted), nil
		}
		return absent(clock.Now()), nil
	}
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: tasks})
	scan(t, ctx, l)
	receive(t, observed)
	finish(t, ctx, l)
	clock.set(created.Add(2*time.Minute + 100*time.Millisecond))
	scan(t, ctx, l)
	if l.reconciling != prior.TaskID {
		t.Fatal("fresh due tick reused the earlier idle observation")
	}
	if got := receive(t, observed); got != prior.TaskID {
		t.Fatalf("observed %s instead of blocked occurrence %s", got, prior.TaskID)
	}
	finish(t, ctx, l)
	scan(t, ctx, l)
	request := receive(t, started)
	if request.TaskID == prior.TaskID {
		t.Fatal("new occurrence reused the prior task identity")
	}
	stored, err := store.GetOccurrence(ctx, prior.TaskID)
	if err != nil || stored.Release == nil {
		t.Fatalf("due observation did not release prior reservation: %+v %v", stored, err)
	}
}

func TestSchedulerObservationCadenceDoesNotRequireAnUninterruptedIdleWait(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	prior := claimPrior(t, store, 1, 91, created, created.Add(time.Minute))
	if err := store.ChangeState(context.Background(), contract.StateChange{
		ScheduleID: id(1), From: contract.StateEnabled, To: contract.StatePaused,
	}); err != nil {
		t.Fatal(err)
	}
	start := created.Add(90 * time.Second)
	clock := newClock(start)
	observed := make(chan string, 2)
	tasks := blockedTasks(t.Context(), make(chan DispatchRequest, 1))
	tasks.observe = func(taskID string) (TaskState, error) {
		observed <- taskID
		return pending(clock.Now(), task.AdmissionAdmitted), nil
	}
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: tasks})
	scan(t, ctx, l)
	receive(t, observed)
	finish(t, ctx, l)
	// Result-driven scans can repeatedly interrupt the idle timer. They must
	// neither spin observations at unchanged time nor prevent later revisits.
	for step := 1; step <= 4; step++ {
		clock.set(start.Add(time.Duration(step) * 200 * time.Millisecond))
		scan(t, ctx, l)
		if l.reconciling != "" {
			t.Fatal("idle occurrence was observed again before its cadence elapsed")
		}
	}
	clock.set(start.Add(time.Second))
	scan(t, ctx, l)
	if l.reconciling != prior.TaskID {
		t.Fatal("observation cadence depended on completing an idle wait")
	}
	receive(t, observed)
	finish(t, ctx, l)
}
