package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type controlledClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan clockWait
}

type clockWait struct {
	ctx      context.Context
	wake     chan struct{}
	duration time.Duration
}

func newClock(now time.Time) *controlledClock {
	return &controlledClock{now: now, waits: make(chan clockWait, 8)}
}

func (c *controlledClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *controlledClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func (c *controlledClock) Wait(ctx context.Context, duration time.Duration) error {
	wait := clockWait{ctx: ctx, wake: make(chan struct{}), duration: duration}
	select {
	case c.waits <- wait:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-wait.wake:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type taskFuncs struct {
	dispatch func(DispatchRequest) (TaskState, error)
	observe  func(string) (TaskState, error)
}

func (f taskFuncs) Dispatch(request DispatchRequest) (TaskState, error) {
	return f.dispatch(request)
}

func (f taskFuncs) Observe(id string) (TaskState, error) {
	return f.observe(id)
}

func instant(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed.UTC()
}

func id(number int) string { return fmt.Sprintf("%032x", number) }

func database(t *testing.T) contract.Store {
	t.Helper()
	store, _, _ := databaseWithOwner(t)
	return store
}

func databaseWithOwner(t *testing.T) (contract.Store, *taskdir.Store, *scheduler.OwnerLease) {
	t.Helper()
	ctx := context.Background()
	taskStore, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := taskStore.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, err := scheduler.AcquireOwnerLease(ctx, taskStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = lease.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	store, err := scheduler.Open(ctx, taskStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store, taskStore, lease
}

func definition(number int, created time.Time) contract.Definition {
	return contract.Definition{DefinitionHeader: contract.DefinitionHeader{
		ScheduleID: id(number), CreatedAt: created, Cron: "* * * * *", Timezone: "UTC",
		Request: contract.Request{Auto: true, CanonicalCwd: "/work/repository", Config: task.TaskConfig{Permission: "read-only"}},
	}, Brief: []byte("check the work")}
}

func create(t *testing.T, store contract.Store, number int, created time.Time) {
	t.Helper()
	if err := store.CreateSchedule(context.Background(), definition(number, created)); err != nil {
		t.Fatal(err)
	}
}

func testLoop(t *testing.T, config Config) (*loop, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	counter := 1000
	config.NewTaskID = func() (string, error) { counter++; return id(counter), nil }
	l := &loop{
		config: config, startup: config.Clock.Now(), live: make(map[string]bool), held: make(map[string]time.Time),
		observedRound: make(map[string]uint64), observedDue: make(map[string]time.Time), results: make(chan taskResult, AdmissionLimit+1),
	}
	t.Cleanup(func() { cancel(); l.workers.Wait() })
	return l, ctx
}

func scan(t *testing.T, ctx context.Context, l *loop) {
	t.Helper()
	if err := l.scan(ctx); err != nil {
		t.Fatal(err)
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for ownership gate")
	}
	var zero T
	return zero
}

func finish(t *testing.T, ctx context.Context, l *loop) {
	t.Helper()
	if err := l.finish(ctx, receive(t, l.results)); err != nil {
		t.Fatal(err)
	}
}

func pending(now time.Time, admission task.Admission) TaskState {
	return TaskState{Observation: contract.Observation{
		ObservedAt: now, Admission: admission,
		Liveness: task.LivenessUndetermined, Publication: task.PublicationPending,
	}}
}

func absent(now time.Time) TaskState {
	state := pending(now, task.AdmissionNotAdmitted)
	state.Release = &contract.Release{ReleasedAt: now, Kind: contract.ReleaseNotCreated, Ref: "task_absent"}
	return state
}

// The test owns this context. It settles unfinished fixture operations before
// cleanup joins the loop; stopping the service never cancels this owner.
func blockedTasks(ownerCtx context.Context, started chan<- DispatchRequest) taskFuncs {
	return taskFuncs{dispatch: func(request DispatchRequest) (TaskState, error) {
		started <- request
		<-ownerCtx.Done()
		return TaskState{}, ownerCtx.Err()
	}, observe: func(string) (TaskState, error) {
		<-ownerCtx.Done()
		return TaskState{}, ownerCtx.Err()
	}}
}

func history(t *testing.T, store contract.Store, scheduleID string) []contract.HistoryEntry {
	t.Helper()
	result, err := store.History(context.Background(), contract.HistoryPage{ScheduleID: scheduleID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return result.Entries
}

func claimPrior(t *testing.T, store contract.Store, number, taskNumber int, created, due time.Time) contract.Occurrence {
	t.Helper()
	claim := contract.Claim{ScheduleID: id(number), ExpectedCursor: created, TaskID: id(taskNumber), Due: due, ArmedAt: due}
	result, err := store.ClaimAndArm(context.Background(), claim)
	if err != nil || !result.AuthorizesInvocation() {
		t.Fatalf("prior claim: %+v %v", result, err)
	}
	return claimOccurrence(claim)
}

func assertSameClaim(t *testing.T, left, right contract.Claim) {
	t.Helper()
	if left.TaskID != right.TaskID || left.ScheduleID != right.ScheduleID ||
		!left.ExpectedCursor.Equal(right.ExpectedCursor) || !left.Due.Equal(right.Due) || !left.ArmedAt.Equal(right.ArmedAt) {
		t.Fatalf("claim retry changed immutable candidate: %+v -> %+v", left, right)
	}
}

func assertOwnerHeld(t *testing.T, store *taskdir.Store) {
	t.Helper()
	lease, err := scheduler.AcquireOwnerLease(t.Context(), store)
	if err == nil {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("scheduler owner lease released before its invocation settled")
	}
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("checking held owner lease: %v", err)
	}
}
