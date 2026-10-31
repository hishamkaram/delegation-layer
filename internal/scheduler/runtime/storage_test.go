package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type mutationStore struct {
	contract.Store
	release     func(context.Context, contract.OccurrenceRelease) error
	observation func(context.Context, contract.OccurrenceObservation) error
	skip        func(context.Context, contract.Skip) error
}

func (s mutationStore) Release(ctx context.Context, candidate contract.OccurrenceRelease) error {
	if s.release != nil {
		return s.release(ctx, candidate)
	}
	return s.Store.Release(ctx, candidate)
}

func (s mutationStore) RecordObservation(ctx context.Context, candidate contract.OccurrenceObservation) error {
	if s.observation != nil {
		return s.observation(ctx, candidate)
	}
	return s.Store.RecordObservation(ctx, candidate)
}

func (s mutationStore) RecordSkip(ctx context.Context, candidate contract.Skip) error {
	if s.skip != nil {
		return s.skip(ctx, candidate)
	}
	return s.Store.RecordSkip(ctx, candidate)
}

func TestSchedulerReleaseReadbackPreservesImmutableWinner(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "conflicting"}[conflict], func(t *testing.T) {
			store := database(t)
			created := instant("2026-01-01T00:00:00Z")
			create(t, store, 1, created)
			prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
			clock := newClock(created.Add(90 * time.Second))
			state := absent(clock.Now())
			state.Release.Kind = contract.ReleaseClosed
			state.Release.Ref = strings.Repeat("a", 64)
			winner := *state.Release
			if conflict {
				winner.Ref = strings.Repeat("b", 64)
			}
			calls := 0
			wrapped := mutationStore{Store: store, release: func(ctx context.Context, candidate contract.OccurrenceRelease) error {
				calls++
				candidate.Release = winner
				if err := store.Release(ctx, candidate); err != nil {
					return err
				}
				if conflict {
					return contract.ErrConflict
				}
				return errors.New("lost release acknowledgement")
			}}
			l, ctx := testLoop(t, Config{Store: wrapped, Clock: clock})
			if err := l.finish(ctx, taskResult{occurrence: prior, state: state}); err != nil {
				t.Fatal(err)
			}
			stored, err := store.GetOccurrence(ctx, prior.TaskID)
			if err != nil || stored.Release == nil || stored.Release.Ref != winner.Ref || calls != 1 {
				t.Fatalf("winner=%+v calls=%d error=%v", stored, calls, err)
			}
		})
	}
}

func TestSchedulerAbsentReleaseRetriesCapturedCandidate(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	clock := newClock(created.Add(90 * time.Second))
	var candidates []contract.OccurrenceRelease
	wrapped := mutationStore{Store: store, release: func(ctx context.Context, candidate contract.OccurrenceRelease) error {
		candidates = append(candidates, candidate)
		if len(candidates) == 1 {
			clock.set(clock.Now().Add(time.Second))
			return errors.New("uncommitted release")
		}
		return store.Release(ctx, candidate)
	}}
	l, ctx := testLoop(t, Config{Store: wrapped, Clock: clock})
	if err := l.finish(ctx, taskResult{occurrence: prior, state: absent(clock.Now())}); err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("release retry count: %+v", candidates)
	}
	left, right := candidates[0], candidates[1]
	if left.TaskID != right.TaskID || left.Release.Kind != right.Release.Kind || left.Release.Ref != right.Release.Ref || !left.Release.ReleasedAt.Equal(right.Release.ReleasedAt) {
		t.Fatalf("release candidate changed after readback: %+v", candidates)
	}
}

func TestSchedulerObservationReadbackRespectsNewerStoreCache(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	clock := newClock(created.Add(90 * time.Second))
	calls := 0
	winner := pending(clock.Now().Add(time.Second), task.AdmissionAdmitted).Observation
	wrapped := mutationStore{Store: store, observation: func(ctx context.Context, candidate contract.OccurrenceObservation) error {
		calls++
		candidate.Observation = winner
		if err := store.RecordObservation(ctx, candidate); err != nil {
			return err
		}
		return errors.New("lost observation acknowledgement")
	}}
	l, ctx := testLoop(t, Config{Store: wrapped, Clock: clock})
	if err := l.finish(ctx, taskResult{occurrence: prior, state: pending(clock.Now(), task.AdmissionUnknown)}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetOccurrence(ctx, prior.TaskID)
	if err != nil || stored.Observation == nil || stored.Observation.Admission != task.AdmissionAdmitted || calls != 1 {
		t.Fatalf("observation winner=%+v calls=%d %v", stored, calls, err)
	}
}

func TestSchedulerSkipPauseRaceAndJoinedStorageFault(t *testing.T) {
	for _, storageFault := range []bool{false, true} {
		t.Run(map[bool]string{false: "pause", true: "joined_fault"}[storageFault], func(t *testing.T) {
			store := database(t)
			created := instant("2026-01-01T00:00:00Z")
			create(t, store, 1, created)
			clock := newClock(created.Add(time.Minute))
			fault := errors.New("storage fault")
			wrapped := mutationStore{Store: store, skip: func(ctx context.Context, skip contract.Skip) error {
				if err := store.ChangeState(ctx, contract.StateChange{ScheduleID: skip.ScheduleID, From: contract.StateEnabled, To: contract.StatePaused}); err != nil {
					return err
				}
				if storageFault {
					return errors.Join(contract.ErrConflict, fault)
				}
				return store.RecordSkip(ctx, skip)
			}}
			l, ctx := testLoop(t, Config{Store: wrapped, Clock: clock, Tasks: blockedTasks(t.Context(), make(chan DispatchRequest, 4))})
			err := l.scan(ctx)
			if storageFault {
				if !errors.Is(err, fault) {
					t.Fatalf("joined storage fault hidden: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(history(t, store, id(1))) != 0 {
				t.Fatal("skip advanced a paused schedule")
			}
		})
	}
}

func TestSchedulerInvariantFaultRemainsFatalAfterMatchingReadback(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	clock := newClock(created.Add(90 * time.Second))
	wrapped := mutationStore{Store: store, observation: func(ctx context.Context, candidate contract.OccurrenceObservation) error {
		if err := store.RecordObservation(ctx, candidate); err != nil {
			return err
		}
		return task.ErrInvariantFault
	}}
	l, ctx := testLoop(t, Config{Store: wrapped, Clock: clock})
	err := l.finish(ctx, taskResult{occurrence: prior, state: pending(clock.Now(), task.AdmissionUnknown)})
	if !errors.Is(err, task.ErrInvariantFault) {
		t.Fatalf("invariant fault hidden by matching cache: %v", err)
	}
	l.config.Store = mutationStore{Store: store, release: func(ctx context.Context, candidate contract.OccurrenceRelease) error {
		if releaseErr := store.Release(ctx, candidate); releaseErr != nil {
			return releaseErr
		}
		return task.ErrInvariantFault
	}}
	err = l.finish(ctx, taskResult{occurrence: prior, state: absent(clock.Now())})
	if !errors.Is(err, task.ErrInvariantFault) {
		t.Fatalf("invariant fault hidden by matching release: %v", err)
	}
}
