package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type claimStore struct {
	contract.Store
	claim func(context.Context, contract.Claim) (contract.ClaimResult, error)
}

func (s claimStore) ClaimAndArm(ctx context.Context, claim contract.Claim) (contract.ClaimResult, error) {
	return s.claim(ctx, claim)
}

func TestSchedulerUncertainCommittedClaimOnlyObserves(t *testing.T) {
	for _, outcome := range []contract.ClaimOutcome{contract.ClaimUnknown, contract.ClaimExisting} {
		t.Run(map[contract.ClaimOutcome]string{contract.ClaimUnknown: "unknown", contract.ClaimExisting: "existing"}[outcome], func(t *testing.T) {
			store := database(t)
			created := instant("2026-01-01T00:00:00Z")
			create(t, store, 1, created)
			clock := newClock(created.Add(30 * time.Second))
			calls := 0
			wrapped := claimStore{Store: store, claim: func(ctx context.Context, candidate contract.Claim) (contract.ClaimResult, error) {
				calls++
				result, err := store.ClaimAndArm(ctx, candidate)
				if err != nil {
					return result, err
				}
				if outcome == contract.ClaimExisting {
					return contract.ClaimResult{Outcome: outcome, TaskID: candidate.TaskID}, nil
				}
				return contract.ClaimResult{}, errors.New("lost commit acknowledgement")
			}}
			started := make(chan DispatchRequest, 4)
			observed := make(chan string, 2)
			tasks := blockedTasks(t.Context(), started)
			tasks.observe = func(taskID string) (TaskState, error) {
				observed <- taskID
				return absent(clock.Now()), nil
			}
			l, ctx := testLoop(t, Config{Store: wrapped, Clock: clock, Tasks: tasks})
			clock.set(created.Add(time.Minute))
			scan(t, ctx, l)
			scan(t, ctx, l)
			taskID := receive(t, observed)
			finish(t, ctx, l)
			stored, err := store.GetOccurrence(ctx, taskID)
			if err != nil || stored.Release == nil || calls != 1 || len(l.live) != 0 {
				t.Fatalf("readback=%+v error=%v claims=%d live=%d", stored, err, calls, len(l.live))
			}
			select {
			case paid := <-started:
				t.Fatalf("readback authorized paid dispatch: %+v", paid)
			default:
			}
		})
	}
}

func TestSchedulerAbsentClaimRetriesOnlyExactCandidateOnce(t *testing.T) {
	for _, commitRetry := range []bool{true, false} {
		t.Run(map[bool]string{true: "retry_commits", false: "retry_absent"}[commitRetry], func(t *testing.T) {
			testAbsentClaimRetry(t, commitRetry)
		})
	}
}

func testAbsentClaimRetry(t *testing.T, commitRetry bool) {
	t.Helper()
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	clock := newClock(created.Add(30 * time.Second))
	var candidates []contract.Claim
	fault := errors.New("uncertain transaction")
	wrapped := claimStore{Store: store, claim: func(ctx context.Context, claim contract.Claim) (contract.ClaimResult, error) {
		candidates = append(candidates, claim)
		if len(candidates) == 2 && commitRetry {
			return store.ClaimAndArm(ctx, claim)
		}
		return contract.ClaimResult{}, fault
	}}
	started := make(chan DispatchRequest, 4)
	l, ctx := testLoop(t, Config{Store: wrapped, Clock: clock, Tasks: blockedTasks(t.Context(), started)})
	clock.set(created.Add(time.Minute))
	err := l.scan(ctx)
	if len(candidates) != 2 {
		t.Fatalf("retry changed candidate count: %+v", candidates)
	}
	assertSameClaim(t, candidates[0], candidates[1])
	if commitRetry {
		if err != nil {
			t.Fatal(err)
		}
		receive(t, started)
	} else {
		if !errors.Is(err, fault) {
			t.Fatalf("unresolved store fault: %v", err)
		}
		if len(l.live) != 0 {
			t.Fatal("observation-only retired attempt retained a live marker")
		}
	}
}

func TestSchedulerLiveMarkerPrecedesClaimAndExcludesReconciliation(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	clock := newClock(created.Add(30 * time.Second))
	started := make(chan DispatchRequest, 4)
	observed := make(chan string, 4)
	tasks := blockedTasks(t.Context(), started)
	tasks.observe = func(taskID string) (TaskState, error) {
		observed <- taskID
		return absent(clock.Now()), nil
	}
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: tasks})
	l.config.Store = claimStore{Store: store, claim: func(ctx context.Context, claim contract.Claim) (contract.ClaimResult, error) {
		if !l.live[claim.TaskID] {
			return contract.ClaimResult{}, task.ErrInvariantFault
		}
		return store.ClaimAndArm(ctx, claim)
	}}
	clock.set(created.Add(time.Minute))
	scan(t, ctx, l)
	receive(t, started)
	for range 3 {
		l.observationRound++
		scan(t, ctx, l)
	}
	select {
	case taskID := <-observed:
		t.Fatalf("observed live dispatch %s", taskID)
	default:
	}
}

func TestSchedulerPauseRaceConflictsAreNotFatalOrDuplicate(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	clock := newClock(created.Add(30 * time.Second))
	wrapped := claimStore{Store: store, claim: func(ctx context.Context, claim contract.Claim) (contract.ClaimResult, error) {
		err := store.ChangeState(ctx, contract.StateChange{ScheduleID: claim.ScheduleID, From: contract.StateEnabled, To: contract.StatePaused})
		if err != nil {
			return contract.ClaimResult{}, err
		}
		return store.ClaimAndArm(ctx, claim)
	}}
	started := make(chan DispatchRequest, 4)
	l, ctx := testLoop(t, Config{Store: wrapped, Clock: clock, Tasks: blockedTasks(t.Context(), started)})
	clock.set(created.Add(time.Minute))
	scan(t, ctx, l)
	scan(t, ctx, l)
	if len(history(t, store, id(1))) != 0 {
		t.Fatal("pause race armed or skipped a tick")
	}
	select {
	case request := <-started:
		t.Fatalf("paused schedule dispatched: %+v", request)
	default:
	}
}
