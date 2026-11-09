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

type resultReaderFunc func(context.Context, contract.Occurrence) (contract.ResultFact, error)

func (f resultReaderFunc) ReadResult(ctx context.Context, o contract.Occurrence) (contract.ResultFact, error) {
	return f(ctx, o)
}

func TestQuotaProjectionPrecedesReleaseAndRecordsOneExpirySpan(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	reliability, ok := store.(contract.ReliabilityStore)
	if !ok {
		t.Fatal("production store lacks reliability port")
	}
	policy := contract.DefaultQuotaPolicy()
	policy.Enabled = true
	if err := reliability.SetQuotaPolicy(t.Context(), id(1), policy); err != nil {
		t.Fatal(err)
	}
	due := created.Add(time.Minute)
	prior := claimPrior(t, store, 1, 90, created, due)
	until := due.Add(time.Hour)
	clock := newClock(due.Add(time.Second))
	reader := resultReaderFunc(func(_ context.Context, o contract.Occurrence) (contract.ResultFact, error) {
		return contract.ResultFact{TaskID: o.TaskID, ObservedAt: clock.Now(), Verdict: "rejected", Code: "provider_quota_exhausted", OutcomeSHA256: strings.Repeat("a", 64), QuotaNotBefore: until}, nil
	})
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: blockedTasks(t.Context(), make(chan DispatchRequest, 4)), Evidence: reader})
	state := TaskState{Observation: contract.Observation{ObservedAt: clock.Now(), Admission: task.AdmissionAdmitted, Publication: task.PublicationRejected, OutcomeSHA256: strings.Repeat("a", 64)}, Release: &contract.Release{ReleasedAt: clock.Now(), Kind: contract.ReleaseClosed, Ref: strings.Repeat("b", 64)}}
	if err := l.finish(ctx, taskResult{occurrence: prior, state: state}); err != nil {
		t.Fatal(err)
	}
	hold, err := reliability.QuotaHold(ctx, id(1))
	if err != nil || hold == nil || !hold.Until.Equal(until) {
		t.Fatalf("reservation released before quota installed: %+v %v", hold, err)
	}
	assertHeldAdmission(t, store, due, clock.Now())
	assertCompactQuotaExpiry(t, l, ctx, store, clock, due, until)
}

func TestAdmissionFailurePersistsBeforeShutdownDropsResult(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	clock := newClock(created.Add(30 * time.Second))
	waker := newWaker()
	dispatched := make(chan string, 1)
	settle := make(chan struct{})
	diagnostic := contract.AttemptFailure{Code: "supervisor_upgrade_required", Stage: "start-supervisor", NextAction: "run_supervisor_migrate", Message: "Supervisor migration required."}
	tasks := blockedTasks(t.Context(), make(chan DispatchRequest, 4))
	tasks.dispatch = func(r DispatchRequest) (TaskState, error) {
		dispatched <- r.TaskID
		select {
		case <-settle:
		case <-t.Context().Done():
			return TaskState{}, t.Context().Err()
		}
		return TaskState{Attempt: &diagnostic}, ErrTaskCleanupUnresolved
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- Run(ctx, Config{Store: store, Clock: clock, Waker: waker, Tasks: tasks})
	}()
	t.Cleanup(func() { cancel(); receive(t, finished) })
	wait := nextWait(t, waker)
	clock.set(created.Add(time.Minute))
	close(wait.wake)
	taskID := receive(t, dispatched)
	cancel()
	close(settle)
	if err := receive(t, done); !errors.Is(err, ErrTaskCleanupUnresolved) {
		t.Fatalf("lost cleanup failure %v", err)
	}
	o, err := store.GetOccurrence(context.Background(), taskID)
	if err != nil || o.Attempt == nil || *o.Attempt != diagnostic || o.Release != nil {
		t.Fatalf("shutdown lost diagnostic or released unsafe task: %+v %v", o, err)
	}
}

func assertHeldAdmission(t *testing.T, store contract.Store, due, now time.Time) {
	t.Helper()
	_, err := store.ClaimAndArm(t.Context(), contract.Claim{ScheduleID: id(1), ExpectedCursor: due, Due: due.Add(time.Minute), TaskID: id(91), ArmedAt: now})
	if !errors.Is(err, contract.ErrConflict) {
		t.Fatalf("next admission bypassed hold %v", err)
	}
}

func assertCompactQuotaExpiry(t *testing.T, l *loop, ctx context.Context, store contract.Store, clock *controlledClock, due, until time.Time) {
	t.Helper()
	rows, err := store.ListSchedules(ctx, contract.SchedulePage{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	summary := rows.Schedules[0]
	for range 3 {
		proceed, skipErr := l.quotaSkip(ctx, &summary)
		if skipErr != nil || proceed {
			t.Fatalf("active hold not held %v %v", proceed, skipErr)
		}
	}
	h, err := store.History(ctx, contract.HistoryPage{ScheduleID: id(1), Limit: 10})
	if err != nil || len(h.Entries) != 1 {
		t.Fatalf("hold fragmented %+v %v", h, err)
	}
	clock.set(until)
	proceed, err := l.quotaSkip(ctx, &summary)
	if err != nil || !proceed {
		t.Fatalf("expiry did not advance %v %v", proceed, err)
	}
	h, err = store.History(ctx, contract.HistoryPage{ScheduleID: id(1), Limit: 10})
	if err != nil || len(h.Entries) != 2 {
		t.Fatalf("expected one expiry span %+v %v", h, err)
	}
	span, ok := h.Entries[0].Skip()
	if !ok || span.Reason != contract.SkipQuotaExhausted || !span.From.Equal(due) || !span.To.Equal(until) {
		t.Fatalf("wrong quota interval %+v", span)
	}
}

func TestSchedulerShutdownJoinsPassiveReconciliationMailbox(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	clock := newClock(created.Add(90 * time.Second))
	started := make(chan struct{}, 1)
	reader := resultReaderFunc(func(ctx context.Context, _ contract.Occurrence) (contract.ResultFact, error) {
		started <- struct{}{}
		<-ctx.Done()
		return contract.ResultFact{}, ctx.Err()
	})
	tasks := taskFuncs{dispatch: func(DispatchRequest) (TaskState, error) { return TaskState{}, errors.New("unexpected dispatch") }, observe: func(string) (TaskState, error) { return pending(clock.Now(), task.AdmissionUnknown), nil }}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		defer close(finished)
		done <- Run(ctx, Config{Store: store, Tasks: tasks, Clock: clock, Evidence: reader})
	}()
	t.Cleanup(func() { cancel(); receive(t, finished) })
	receive(t, started)
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatalf("reconciliation shutdown failed: %v", err)
	}
}

type hookRunnerFunc func(context.Context, contract.FailureHook, contract.Notification) (string, error)

func (f hookRunnerFunc) RunHook(ctx context.Context, h contract.FailureHook, n contract.Notification) (string, error) {
	return f(ctx, h, n)
}

func TestPersistedProjectionStillClaimsNotificationAfterRestart(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	d := definition(1, created)
	d.Hook = &contract.FailureHook{Executable: "/installed/hook", Threshold: 1, Timeout: time.Second}
	if err := store.CreateSchedule(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	reliability, ok := store.(contract.ReliabilityStore)
	if !ok {
		t.Fatal("missing production reliability store")
	}
	if err := reliability.RecordResult(t.Context(), contract.ResultFact{TaskID: prior.TaskID, ObservedAt: created.Add(61 * time.Second), Verdict: "not_created", Code: "admission_failed"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	reader := resultReaderFunc(func(context.Context, contract.Occurrence) (contract.ResultFact, error) {
		t.Error("immutable terminal evidence was unnecessarily reread")
		return contract.ResultFact{}, errors.New("unexpected evidence read")
	})
	runner := hookRunnerFunc(func(_ context.Context, _ contract.FailureHook, n contract.Notification) (string, error) {
		calls++
		if n.Kind != "failure" || n.StreakCount != 1 {
			t.Errorf("wrong notice %+v", n)
		}
		return "succeeded", nil
	})
	l, ctx := testLoop(t, Config{Store: store, Clock: newClock(created.Add(90 * time.Second)), Evidence: reader, Hooks: runner, NewNotificationID: func() (string, error) { return id(100), nil }})
	for range 2 {
		if result := l.reconcilePage(ctx, reliability, "", ""); result.err != nil {
			t.Fatal(result.err)
		}
	}
	if calls != 1 {
		t.Fatalf("restart lost or duplicated notification: %d", calls)
	}
}
