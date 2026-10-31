package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSchedulerDueObservationPriorityAndFairIdleRevisit(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	for number := 1; number <= 3; number++ {
		create(t, store, number, created)
		claimPrior(t, store, number, 90+number, created, created.Add(time.Minute))
	}
	if err := store.ChangeState(context.Background(), contract.StateChange{ScheduleID: id(1), From: contract.StateEnabled, To: contract.StateDeleted}); err != nil {
		t.Fatal(err)
	}
	if err := store.ChangeState(context.Background(), contract.StateChange{ScheduleID: id(3), From: contract.StateEnabled, To: contract.StatePaused}); err != nil {
		t.Fatal(err)
	}
	clock := newClock(created.Add(90 * time.Second))
	observed := make(chan string, 4)
	gate := make(chan struct{}, 4)
	ownerCtx := t.Context()
	tasks := blockedTasks(t.Context(), make(chan DispatchRequest, 4))
	tasks.observe = func(taskID string) (TaskState, error) {
		observed <- taskID
		select {
		case <-gate:
			return pending(clock.Now(), task.AdmissionAdmitted), nil
		case <-ownerCtx.Done():
			return TaskState{}, ownerCtx.Err()
		}
	}
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: tasks})
	clock.set(created.Add(2 * time.Minute))
	scan(t, ctx, l)
	if got := receive(t, observed); got != id(92) {
		t.Fatalf("idle observation beat due reservation: %s", got)
	}
	for range 2 {
		scan(t, ctx, l)
	}
	select {
	case duplicate := <-observed:
		t.Fatalf("concurrent reconciliation %s", duplicate)
	default:
	}
	gate <- struct{}{}
	finish(t, ctx, l)
	// Due occurrence is already visited this round. Idle rotation continues
	// through deleted and paused definitions, without waiting for task closure.
	for _, want := range []string{id(93), id(91)} {
		scan(t, ctx, l)
		if got := receive(t, observed); got != want {
			t.Fatalf("idle rotation=%s want %s", got, want)
		}
		gate <- struct{}{}
		finish(t, ctx, l)
	}
	l.observationRound++
	scan(t, ctx, l)
	if got := receive(t, observed); got != id(92) {
		t.Fatalf("unresolved occurrence not revisited: %s", got)
	}
}

func TestSchedulerHeldTickRetainsLastValidatedAdmissionAcrossRollback(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	clock := newClock(created.Add(90 * time.Second))
	gate := make(chan struct{})
	ownerCtx := t.Context()
	tasks := blockedTasks(t.Context(), make(chan DispatchRequest, 4))
	tasks.observe = func(string) (TaskState, error) {
		select {
		case <-gate:
			return pending(clock.Now(), task.AdmissionAdmitted), nil
		case <-ownerCtx.Done():
			return TaskState{}, ownerCtx.Err()
		}
	}
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: tasks})
	clock.set(created.Add(2 * time.Minute))
	scan(t, ctx, l)
	close(gate)
	finish(t, ctx, l)
	stored, err := store.GetOccurrence(ctx, prior.TaskID)
	if err != nil || stored.Observation == nil || stored.Observation.Admission != task.AdmissionAdmitted {
		t.Fatalf("validated observation: %+v %v", stored, err)
	}
	clock.set(created.Add(90 * time.Second))
	scan(t, ctx, l)
	clock.set(created.Add(2*time.Minute + 5*time.Second))
	scan(t, ctx, l)
	span, ok := history(t, store, id(1))[0].Skip()
	if !ok || span.Reason != contract.SkipOverlapActive {
		t.Fatalf("rollback lost fresh candidate/admission: %+v", span)
	}
}
