package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/pulse"
)

// joiningWaker holds a completed production wait until the owner permits its
// return. The loop must join that return before touching a result or exiting.
type joiningWaker struct {
	inner    Waker
	entered  chan context.Context
	returned chan struct{}
	release  <-chan struct{}
	fault    error
}

func (w joiningWaker) Wait(ctx context.Context) error {
	w.entered <- ctx
	err := w.inner.Wait(ctx)
	close(w.returned)
	<-w.release
	return errors.Join(err, w.fault)
}

func TestSchedulerWaitJoinsProductionWakerOnResultAndCancellation(t *testing.T) {
	for _, mode := range []string{"result", "cancellation", "cancellation_with_fault"} {
		t.Run(mode, func(t *testing.T) { testWaitJoin(t, mode) })
	}
}

func testWaitJoin(t *testing.T, mode string) {
	t.Helper()
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
	clock := newClock(created.Add(90 * time.Second))
	source, sourceErr := pulse.New()
	if sourceErr != nil {
		t.Fatal(sourceErr)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	waker := joiningWaker{
		inner: source, entered: make(chan context.Context, 1), returned: make(chan struct{}), release: gate,
	}
	if mode == "cancellation_with_fault" {
		waker.fault = errors.New("wake source fault during cancellation")
	}
	l, parent := testLoop(t, Config{Store: store, Clock: clock, Waker: waker})
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() { defer close(joined); done <- l.wait(ctx) }()
	t.Cleanup(func() { cancel(); release(); receive(t, joined) })
	waitCtx := receive(t, waker.entered)
	if mode == "result" {
		l.results <- taskResult{occurrence: prior, state: absent(clock.Now())}
	} else {
		cancel()
	}
	receive(t, waker.returned)
	if !errors.Is(waitCtx.Err(), context.Canceled) {
		t.Fatalf("loop did not cancel the production wait: %v", waitCtx.Err())
	}
	select {
	case err := <-done:
		t.Fatalf("loop returned without joining its wait: %v", err)
	default:
	}
	assertWakeResultRelease(t, parent, store, prior.TaskID, false)
	release()
	waitErr := receive(t, done)
	if mode == "result" {
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		assertWakeResultRelease(t, parent, store, prior.TaskID, true)
	} else if !errors.Is(waitErr, context.Canceled) || (waker.fault != nil && !errors.Is(waitErr, waker.fault)) {
		t.Fatalf("joined cancellation lost a cause: %v", waitErr)
	}
}

func assertWakeResultRelease(t *testing.T, ctx context.Context, store contract.Store, taskID string, released bool) {
	t.Helper()
	stored, err := store.GetOccurrence(ctx, taskID)
	if err != nil || (stored.Release != nil) != released {
		t.Fatalf("worker result release=%v, want %t: %v", stored.Release, released, err)
	}
}

type wakerFunc func(context.Context) error

func (w wakerFunc) Wait(ctx context.Context) error { return w(ctx) }

func TestSchedulerRunRetainsWakeSourceFault(t *testing.T) {
	fault := errors.New("wake source unavailable")
	clock := newClock(instant("2026-01-01T00:00:00Z"))
	waker := wakerFunc(func(context.Context) error { return fault })
	err := Run(t.Context(), Config{
		Store: database(t), Tasks: blockedTasks(t.Context(), make(chan DispatchRequest, 1)), Clock: clock, Waker: waker,
	})
	if !errors.Is(err, fault) {
		t.Fatalf("Run hid wake source fault: %v", err)
	}
}

// The standalone path accepts a clock that cannot wait. A second scan must come
// from the default Waker rather than from business wall time or task results.
type scanCountingStore struct {
	contract.Store
	scans  int
	cancel context.CancelFunc
}

func (s *scanCountingStore) ActiveOccurrences(ctx context.Context, page contract.ActivePage) (contract.ActivePageResult, error) {
	s.scans++
	if s.scans == 2 {
		s.cancel()
	}
	return s.Store.ActiveOccurrences(ctx, page)
}

func TestSchedulerStandaloneRunDefaultsWaker(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	store := &scanCountingStore{Store: database(t), cancel: cancel}
	clock := newClock(instant("2026-01-01T00:00:00Z"))
	started := time.Now()
	err := Run(ctx, Config{
		Store: store, Tasks: blockedTasks(t.Context(), make(chan DispatchRequest, 1)), Clock: clock,
	})
	if err != nil || store.scans != 2 || time.Since(started) < IdleInterval {
		t.Fatalf("standalone timer wake: scans=%d elapsed=%s err=%v", store.scans, time.Since(started), err)
	}
}
