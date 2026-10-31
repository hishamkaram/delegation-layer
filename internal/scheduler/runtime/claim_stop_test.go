package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

func TestSchedulerCommittedClaimDispatchesDespiteStopAfterCommit(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	clock := newClock(created.Add(time.Minute))
	started := make(chan DispatchRequest, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wrapped := claimStore{Store: store, claim: func(claimCtx context.Context, candidate contract.Claim) (contract.ClaimResult, error) {
		result, err := store.ClaimAndArm(claimCtx, candidate)
		if err == nil {
			cancel() // Deterministically stop after commit, before its caller resumes.
		}
		return result, err
	}}
	tasks := blockedTasks(t.Context(), started)
	tasks.dispatch = func(request DispatchRequest) (TaskState, error) {
		started <- request
		return absent(clock.Now()), nil
	}
	l, _ := testLoop(t, Config{Store: wrapped, Clock: clock, Tasks: tasks})
	page, err := store.ListSchedules(ctx, contract.SchedulePage{Limit: 1})
	if err != nil || len(page.Schedules) != 1 {
		t.Fatalf("schedule fixture: %+v %v", page, err)
	}
	if err = l.admit(ctx, page.Schedules[0], clock.Now()); err != nil {
		t.Fatal(err)
	}
	if l.admissions != 1 {
		t.Fatal("stop discarded an already committed admission")
	}
	request := receive(t, started)
	l.workers.Wait()
	stored, err := store.GetOccurrence(t.Context(), request.TaskID)
	if err != nil || stored.TaskID != request.TaskID || stored.Release != nil {
		t.Fatalf("committed occurrence changed: %+v %v", stored, err)
	}
	select {
	case extra := <-started:
		t.Fatalf("duplicate dispatch: %+v", extra)
	default:
	}
}
