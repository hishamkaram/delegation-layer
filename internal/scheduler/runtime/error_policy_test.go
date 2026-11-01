package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSchedulerObservationDiagnosticPreservesReleaseAndNextDispatch(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	clock := newClock(created.Add(90 * time.Second))
	observed := make(chan string, 1)
	started := make(chan DispatchRequest, 1)
	state := pending(clock.Now(), task.AdmissionAdmitted)
	state.Observation.Liveness = task.LivenessEnded
	state.Observation.FaultCode = "collection_failed"
	state.Release = &contract.Release{
		ReleasedAt: clock.Now(), Kind: contract.ReleaseClosed,
		Ref: task.ComputeSHA256([]byte("validated native closure")),
	}
	diagnostic := errors.New("collection diagnostic after valid local re-read")
	tasks := taskFuncs{
		observe: func(taskID string) (TaskState, error) {
			observed <- taskID
			return state, diagnostic
		},
		dispatch: func(request DispatchRequest) (TaskState, error) {
			started <- request
			return pending(clock.Now(), task.AdmissionAdmitted), nil
		},
	}
	var events []Event
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: tasks, Report: func(event Event) { events = append(events, event) }})
	scan(t, ctx, l)
	if taskID := receive(t, observed); taskID != prior.TaskID {
		t.Fatalf("observed task=%s, want prior %s", taskID, prior.TaskID)
	}
	finish(t, ctx, l)
	assertSchedulerDiagnosticRelease(t, ctx, store, prior.TaskID, state)
	clock.set(created.Add(2 * time.Minute))
	scan(t, ctx, l)
	request := receive(t, started)
	if request.Definition.ScheduleID != prior.ScheduleID || request.TaskID == prior.TaskID {
		t.Fatalf("next occurrence was blocked or reused the old task: %+v", request)
	}
	finish(t, ctx, l)
	next, err := store.GetOccurrence(ctx, request.TaskID)
	if err != nil || !next.Due.Equal(created.Add(2*time.Minute)) {
		t.Fatalf("next eligible minute was not dispatched: %+v %v", next, err)
	}
	for _, event := range events {
		if event.TaskID == prior.TaskID && event.Phase == "observe" && event.Code == "task_error" {
			return
		}
	}
	t.Fatal("release succeeded but task diagnostic was not reported")
}

func assertSchedulerDiagnosticRelease(t *testing.T, ctx context.Context, store contract.Store, taskID string, state TaskState) {
	t.Helper()
	stored, err := store.GetOccurrence(ctx, taskID)
	if err != nil || stored.Release == nil || stored.Release.Kind != state.Release.Kind || stored.Release.Ref != state.Release.Ref || !stored.Release.ReleasedAt.Equal(state.Release.ReleasedAt) {
		t.Fatalf("task diagnostic discarded valid release: %+v %v", stored, err)
	}
	if stored.Observation == nil || !stored.Observation.ObservedAt.Equal(state.Observation.ObservedAt) || stored.Observation.Admission != task.AdmissionAdmitted || stored.Observation.Liveness != task.LivenessEnded || stored.Observation.Publication != task.PublicationPending || stored.Observation.FaultCode != "collection_failed" {
		t.Fatalf("task diagnostic discarded valid observation: %+v", stored.Observation)
	}
}

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
	waker := newWaker()
	tasks := blockedTasks(t.Context(), make(chan DispatchRequest, 1))
	tasks.dispatch = func(DispatchRequest) (TaskState, error) {
		return TaskState{}, errors.Join(ErrRootIdentity, task.ErrIdentityMismatch)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- Run(ctx, Config{Store: store, Tasks: tasks, Clock: clock, Waker: waker})
	}()
	t.Cleanup(func() { cancel(); receive(t, finished) })
	wait := nextWait(t, waker)
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
