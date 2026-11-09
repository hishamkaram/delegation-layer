package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestReleaseProjectionTaskFaultDoesNotStopOtherSchedules(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	create(t, store, 2, created)
	prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	clock := newClock(created.Add(90 * time.Second))
	observed := make(chan string, 1)
	started := make(chan DispatchRequest, 1)
	tasks := taskFuncs{observe: func(taskID string) (TaskState, error) { observed <- taskID; return absent(clock.Now()), nil }, dispatch: func(request DispatchRequest) (TaskState, error) { started <- request; return absent(clock.Now()), nil }}
	reader := resultReaderFunc(func(_ context.Context, o contract.Occurrence) (contract.ResultFact, error) {
		if o.ScheduleID == id(1) {
			return contract.ResultFact{}, task.ErrEvidenceFault
		}
		return contract.ResultFact{TaskID: o.TaskID, ObservedAt: clock.Now(), Verdict: "not_created", Code: "admission_failed"}, nil
	})
	var events []Event
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: tasks, Evidence: reader, Report: func(event Event) { events = append(events, event) }})
	scan(t, ctx, l)
	receive(t, observed)
	finish(t, ctx, l)
	assertReservationHeld(t, ctx, store, prior.TaskID)
	clock.set(created.Add(2 * time.Minute))
	scan(t, ctx, l)
	request := receive(t, started)
	if request.Definition.ScheduleID != id(2) {
		t.Fatalf("affected schedule retried instead of admitting healthy schedule: %+v", request)
	}
	for l.admissions > 0 {
		finish(t, ctx, l)
	}
	assertReservationHeld(t, ctx, store, prior.TaskID)
	healthy, err := store.GetOccurrence(ctx, request.TaskID)
	if err != nil || healthy.Release == nil {
		t.Fatalf("healthy schedule did not finish: %+v %v", healthy, err)
	}
	for _, event := range events {
		if event.TaskID == prior.TaskID && event.Code == "evidence_unavailable" {
			return
		}
	}
	t.Fatal("isolated evidence fault was not reported")
}

func assertReservationHeld(t *testing.T, ctx context.Context, store contract.Store, taskID string) {
	t.Helper()
	stored, err := store.GetOccurrence(ctx, taskID)
	if err != nil || stored.Release != nil {
		t.Fatalf("unreadable evidence released reservation: %+v %v", stored, err)
	}
}

func TestResultReaderCleanupFaultIsFatalInBothPaths(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_release", true: "background"}[background], func(t *testing.T) {
			store := database(t)
			created := instant("2026-01-01T00:00:00Z")
			create(t, store, 1, created)
			prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
			clock := newClock(created.Add(90 * time.Second))
			retained := errors.New("reader cleanup still owns resources")
			reader := resultReaderFunc(func(context.Context, contract.Occurrence) (contract.ResultFact, error) {
				return contract.ResultFact{}, errors.Join(ErrTaskCleanupUnresolved, retained)
			})
			l, ctx := testLoop(t, Config{Store: store, Clock: clock, Evidence: reader})
			var resultErr error
			if background {
				reliability, ok := store.(contract.ReliabilityStore)
				if !ok {
					t.Fatal("missing reliability store")
				}
				resultErr = l.reconcilePage(ctx, reliability, "", "").err
			} else {
				resultErr = l.finish(ctx, taskResult{occurrence: prior, state: absent(clock.Now())})
			}
			if !errors.Is(resultErr, ErrTaskCleanupUnresolved) || !errors.Is(resultErr, retained) {
				t.Fatalf("cleanup owner fault was hidden: %v", resultErr)
			}
			assertReservationHeld(t, ctx, store, prior.TaskID)
		})
	}
}

func TestSelectedReliabilityResultRetiresBeforeWakerJoinFault(t *testing.T) {
	clock := newClock(instant("2026-01-01T00:00:00Z"))
	waker, release := newJoiningWakeFault(t)
	l, parent := testLoop(t, Config{Clock: clock, Waker: waker})
	l.reliabilityResults = make(chan reliabilityResult, 1)
	l.reliabilityRunning = true
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	joined := make(chan struct{})
	t.Cleanup(func() { cancel(); release(); receive(t, joined) })
	go func() { defer close(joined); done <- l.wait(ctx) }()
	receive(t, waker.entered)
	resultFault := errors.New("reconciliation storage failure")
	l.reliabilityResults <- reliabilityResult{err: resultFault}
	// The waker cannot return until wait selects the result and cancels its wait.
	receive(t, waker.returned)
	release()
	if err := receive(t, done); !errors.Is(err, waker.fault) || !errors.Is(err, resultFault) {
		t.Fatalf("lost waker or result fault: %v", err)
	}
	if l.reliabilityRunning {
		t.Fatal("consumed result still marked running; Run shutdown would wait forever")
	}
}
