package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSchedulerStartupMissedSpansAndRollback(t *testing.T) {
	store := database(t)
	created := instant("1926-01-01T00:00:00Z")
	startup := instant("2026-01-01T00:00:02Z")
	create(t, store, 1, created)
	clock := newClock(startup)
	started := make(chan DispatchRequest, 4)
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: blockedTasks(t.Context(), started)})
	scan(t, ctx, l)
	entries := history(t, store, id(1))
	if len(entries) != 1 {
		t.Fatalf("startup must produce one span: %+v", entries)
	}
	span, ok := entries[0].Skip()
	if !ok || !span.From.Equal(created) || !span.To.Equal(startup) || span.Reason != contract.SkipMissedUnavailable {
		t.Fatalf("startup must compact a century into one span: %+v", entries)
	}
	// A forward jump skips only expired ticks, preserving the fresh minute.
	clock.set(instant("2026-01-01T03:00:04Z"))
	scan(t, ctx, l)
	request := receive(t, started)
	occurrence, err := store.GetOccurrence(ctx, request.TaskID)
	if err != nil || !occurrence.Due.Equal(instant("2026-01-01T03:00:00Z")) {
		t.Fatalf("fresh tick: %+v %v", occurrence, err)
	}
	entries = history(t, store, id(1))
	if len(entries) != 3 {
		t.Fatalf("missed ticks were enumerated: %d entries", len(entries))
	}
	clock.set(startup)
	scan(t, ctx, l)
	if len(history(t, store, id(1))) != 3 {
		t.Fatal("clock rollback changed the cursor/history")
	}
	select {
	case extra := <-started:
		t.Fatalf("duplicate dispatch on rollback: %+v", extra)
	default:
	}
}

func TestSchedulerCapacityIsImmediateAndFairAcrossPages(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	for number := 1; number <= 105; number++ {
		create(t, store, number, created)
	}
	clock := newClock(created.Add(30 * time.Second))
	started := make(chan DispatchRequest, 8)
	settle := make(chan struct{}, 4)
	ownerCtx := t.Context()
	tasks := blockedTasks(t.Context(), started)
	tasks.dispatch = func(request DispatchRequest) (TaskState, error) {
		started <- request
		select {
		case <-settle:
			return absent(clock.Now()), nil
		case <-ownerCtx.Done():
			return TaskState{}, ownerCtx.Err()
		}
	}
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: tasks})
	clock.set(created.Add(time.Minute))
	scan(t, ctx, l)
	for range AdmissionLimit {
		receive(t, started)
	}
	if l.admissions != 4 {
		t.Fatalf("admissions=%d, want four live invocations", l.admissions)
	}
	for _, number := range []int{5, 100, 105} {
		entries := history(t, store, id(number))
		span, ok := entries[0].Skip()
		if len(entries) != 1 || !ok || span.Reason != contract.SkipMissedCapacity {
			t.Fatalf("capacity %d: %+v", number, entries)
		}
	}
	for range AdmissionLimit {
		settle <- struct{}{}
	}
	for range AdmissionLimit {
		finish(t, ctx, l)
	}
	clock.set(created.Add(2 * time.Minute))
	scan(t, ctx, l)
	for range AdmissionLimit {
		request := receive(t, started)
		if request.Definition.ScheduleID <= id(4) || request.Definition.ScheduleID > id(8) {
			t.Fatalf("selection did not rotate: %s", request.Definition.ScheduleID)
		}
	}
}

func TestSchedulerFreshReservationHeldUntilExpiry(t *testing.T) {
	for _, admission := range []task.Admission{task.AdmissionUnknown, task.AdmissionAdmitted} {
		t.Run(fmt.Sprint(admission), func(t *testing.T) {
			testHeldReservation(t, admission)
		})
	}
}

func testHeldReservation(t *testing.T, admission task.Admission) {
	t.Helper()
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	if err := store.RecordObservation(context.Background(), contract.OccurrenceObservation{TaskID: prior.TaskID, Observation: pending(created.Add(time.Minute), admission).Observation}); err != nil {
		t.Fatal(err)
	}
	clock := newClock(created.Add(90 * time.Second))
	observed := make(chan string, 2)
	ownerCtx := t.Context()
	tasks := blockedTasks(t.Context(), make(chan DispatchRequest, 4))
	tasks.observe = func(taskID string) (TaskState, error) {
		observed <- taskID
		<-ownerCtx.Done()
		return TaskState{}, ownerCtx.Err()
	}
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: tasks})
	clock.set(created.Add(2 * time.Minute))
	scan(t, ctx, l)
	if got := receive(t, observed); got != prior.TaskID {
		t.Fatalf("observation=%s", got)
	}
	if len(history(t, store, id(1))) != 1 || l.admissions != 0 {
		t.Fatal("fresh blocker consumed capacity or skipped early")
	}
	clock.set(created.Add(2*time.Minute + 5*time.Second))
	scan(t, ctx, l)
	span, ok := history(t, store, id(1))[0].Skip()
	want := contract.SkipBlockedUncertain
	if admission == task.AdmissionAdmitted {
		want = contract.SkipOverlapActive
	}
	if !ok || span.Reason != want {
		t.Fatalf("expiry=%+v, want %s", span, want)
	}
	select {
	case duplicate := <-observed:
		t.Fatalf("second in-flight observation: %s", duplicate)
	default:
	}
}

func TestSchedulerExpiredFirstSeenIgnoresHistoricalBlocker(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	clock := newClock(created.Add(90 * time.Second))
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: blockedTasks(t.Context(), make(chan DispatchRequest, 4))})
	clock.set(created.Add(2*time.Minute + 5*time.Second))
	scan(t, ctx, l)
	span, ok := history(t, store, id(1))[0].Skip()
	if !ok || span.Reason != contract.SkipMissedUnavailable {
		t.Fatalf("first-seen expired tick: %+v", span)
	}
}

func TestSchedulerReleaseWithinWindowAndCapacityAfterRelease(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "full"}[full], func(t *testing.T) {
			testReleasedCapacity(t, full)
		})
	}
}

func testReleasedCapacity(t *testing.T, full bool) {
	t.Helper()
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	clock := newClock(created.Add(90 * time.Second))
	gate := make(chan struct{})
	started := make(chan DispatchRequest, 4)
	ownerCtx := t.Context()
	tasks := blockedTasks(t.Context(), started)
	if full {
		for number := 2; number <= 5; number++ {
			create(t, store, number, created)
		}
	}
	tasks.observe = func(string) (TaskState, error) {
		select {
		case <-gate:
			return absent(clock.Now()), nil
		case <-ownerCtx.Done():
			return TaskState{}, ownerCtx.Err()
		}
	}
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: tasks})
	clock.set(created.Add(2 * time.Minute))
	scan(t, ctx, l)
	clock.set(clock.Now().Add(time.Second))
	close(gate)
	finish(t, ctx, l)
	scan(t, ctx, l)
	stored, err := store.GetOccurrence(ctx, prior.TaskID)
	if err != nil || stored.Release == nil {
		t.Fatalf("release=%+v %v", stored, err)
	}
	if full {
		span, ok := history(t, store, id(1))[0].Skip()
		if !ok || span.Reason != contract.SkipMissedCapacity {
			t.Fatalf("released capacity tick: %+v", span)
		}
	} else {
		receive(t, started)
	}
}

func TestSchedulerTaskErrorReportsAndOtherSchedulesContinue(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	for number := 1; number <= 2; number++ {
		create(t, store, number, created)
	}
	clock := newClock(created.Add(30 * time.Second))
	started := make(chan DispatchRequest, 4)
	tasks := blockedTasks(t.Context(), started)
	tasks.dispatch = func(request DispatchRequest) (TaskState, error) {
		started <- request
		return absent(clock.Now()), errors.New("private provider output")
	}
	var events []Event
	l, ctx := testLoop(t, Config{Store: store, Clock: clock, Tasks: tasks, Report: func(event Event) { events = append(events, event) }})
	clock.set(created.Add(time.Minute))
	scan(t, ctx, l)
	for range 2 {
		receive(t, started)
		finish(t, ctx, l)
	}
	if len(events) < 4 || l.admissions != 0 {
		t.Fatalf("events=%+v admissions=%d", events, l.admissions)
	}
	for _, event := range events {
		if event.Code == "private provider output" {
			t.Fatal("raw diagnostics escaped")
		}
	}
	clock.set(created.Add(2 * time.Minute))
	scan(t, ctx, l)
	for range 2 {
		receive(t, started)
	}
}
