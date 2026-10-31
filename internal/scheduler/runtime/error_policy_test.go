package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSchedulerTaskEvidenceFaultBlocksOnlyAffectedSchedule(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	create(t, store, 2, created)
	clock := newClock(created.Add(30 * time.Second))
	started := make(chan DispatchRequest, AdmissionLimit)
	tasks := blockedTasks(t.Context(), started)
	tasks.dispatch = func(request DispatchRequest) (TaskState, error) {
		started <- request
		if request.Definition.ScheduleID == id(1) {
			state := pending(clock.Now(), task.AdmissionUnknown)
			state.Observation.FaultCode = "blocked_uncertain"
			return state, task.ErrInvariantFault
		}
		return absent(clock.Now()), nil
	}
	var events []Event
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: tasks, Report: func(event Event) { events = append(events, event) }})
	clock.set(created.Add(time.Minute))
	scan(t, ctx, l)
	for range 2 {
		receive(t, started)
		finish(t, ctx, l)
	}
	var bad contract.Occurrence
	for _, entry := range history(t, store, id(1)) {
		if occurrence, ok := entry.Occurrence(); ok {
			bad = occurrence
		}
	}
	if bad.TaskID == "" || bad.Release != nil || bad.Observation == nil || bad.Observation.FaultCode != "blocked_uncertain" {
		t.Fatalf("bad task must retain its uncertain reservation: %+v", bad)
	}
	clock.set(created.Add(2 * time.Minute))
	scan(t, ctx, l)
	request := receive(t, started)
	if request.Definition.ScheduleID != id(2) {
		t.Fatalf("uncertain task retried instead of next healthy occurrence: %+v", request)
	}
	finish(t, ctx, l)
	clock.set(created.Add(2*time.Minute + ClaimWindow))
	scan(t, ctx, l)
	span, ok := history(t, store, id(1))[0].Skip()
	if !ok || span.Reason != contract.SkipBlockedUncertain {
		t.Fatalf("bad task did not block only its schedule: %+v", span)
	}
	for _, event := range events {
		if event.TaskID == bad.TaskID && event.Code == "task_error" {
			return
		}
	}
	t.Fatal("task evidence fault was not reported")
}

func TestSchedulerRootIdentityFaultStopsRun(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	clock := newClock(created.Add(30 * time.Second))
	tasks := blockedTasks(t.Context(), make(chan DispatchRequest, 1))
	tasks.dispatch = func(DispatchRequest) (TaskState, error) {
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
	if err := receive(t, done); !errors.Is(err, ErrRootIdentity) || !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("root identity fault did not stop Run: %v", err)
	}
}

func TestSchedulerInvalidTaskStateRemainsFatal(t *testing.T) {
	for _, invalidRelease := range []bool{false, true} {
		t.Run(map[bool]string{false: "observation", true: "release"}[invalidRelease], func(t *testing.T) {
			store := database(t)
			created := instant("2026-01-01T00:00:00Z")
			create(t, store, 1, created)
			prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
			clock := newClock(created.Add(90 * time.Second))
			state := pending(clock.Now(), task.AdmissionUnknown)
			if invalidRelease {
				state.Release = &contract.Release{ReleasedAt: clock.Now(), Kind: contract.ReleaseClosed, Ref: "invalid"}
			} else {
				state.Observation.Publication = task.PublicationCommitted // Missing the required outcome digest.
			}
			l, ctx := testLoop(t, Config{Store: store, Clock: clock})
			err := l.finish(ctx, taskResult{occurrence: prior, state: state, err: task.ErrInvariantFault})
			if !errors.Is(err, task.ErrInvariantFault) {
				t.Fatalf("invalid task port value accepted: %v", err)
			}
			stored, readErr := store.GetOccurrence(ctx, prior.TaskID)
			if readErr != nil || stored.Release != nil {
				t.Fatalf("invalid value released reservation: %+v %v", stored, readErr)
			}
		})
	}
}
