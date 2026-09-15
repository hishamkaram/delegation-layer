package execution

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPreflightRefusalSealsStartFailedWithoutLaunching(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "echo")
	events := &eventLog{}
	injected := errors.New("compiled profile changed before launch")
	preflightCalls, startCalls := 0, 0

	result := Run(td, permit, plan, Options{
		Preflight: func(PreflightScope) error {
			preflightCalls++
			events.add("preflight")
			return injected
		},
		Hooks: Hooks{
			Event: events.add,
			Start: func(*exec.Cmd) error {
				startCalls++
				events.add("start-hook")
				return errors.New("unexpected provider launch")
			},
		},
	})
	require(t, errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError))
	if result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
		t.Fatalf("preflight refusal was not published as a rejection: %+v", result)
	}
	if preflightCalls != 1 || startCalls != 0 {
		t.Fatalf("preflight=%d start=%d, want one preflight and no launch", preflightCalls, startCalls)
	}
	if got := string(payload(t, td, result.Outcome)); got != "start_failed: "+injected.Error() {
		t.Fatalf("wrong refusal payload: %q", got)
	}
	inspection, err := td.Inspect()
	require(t, err)
	if !inspection.StartExists || !inspection.SealExists || !inspection.OutcomeExists || inspection.StartedExists {
		t.Fatalf("preflight refusal left incomplete authority: %+v", inspection)
	}
	eventsSnapshot := events.snapshot()
	assertOrder(t, eventsSnapshot, "start-entry", "preflight")
	assertOrder(t, eventsSnapshot, "preflight", "start-failed")

	replayed, cleanupErr, collectErr := td.Collect(plan.Predicate)
	require(t, errors.Join(cleanupErr, collectErr))
	if replayed == nil || replayed.Verdict != result.Outcome.Verdict {
		t.Fatalf("sealed refusal did not replay: %+v", replayed)
	}
	if !bytes.Equal(payload(t, td, replayed), payload(t, td, result.Outcome)) {
		t.Fatal("replayed refusal payload changed")
	}
}

func TestPreflightPassesBeforeExactlyOneLaunch(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "echo")
	events := &eventLog{}
	preflightCalls, startCalls := 0, 0

	result := Run(td, permit, plan, Options{
		Preflight: func(PreflightScope) error {
			preflightCalls++
			events.add("preflight")
			return nil
		},
		Hooks: Hooks{
			Event: events.add,
			Start: func(cmd *exec.Cmd) error {
				startCalls++
				events.add("start-hook")
				return cmd.Start()
			},
		},
	})
	assertSuccess(t, result)
	if preflightCalls != 1 || startCalls != 1 {
		t.Fatalf("preflight=%d start=%d, want one each", preflightCalls, startCalls)
	}
	eventsSnapshot := events.snapshot()
	assertOrder(t, eventsSnapshot, "start-entry", "preflight")
	assertOrder(t, eventsSnapshot, "preflight", "start-hook")
	assertOrder(t, eventsSnapshot, "preflight", "start-authorized")
	assertOrder(t, eventsSnapshot, "start-authorized", "start-hook")
	assertOrder(t, eventsSnapshot, "start-hook", "started")
}

func TestTimerExpiryDuringSuccessfulPreflightCannotLaunch(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "echo")
	clock := newManualClock()
	entered := make(chan struct{})
	release := make(chan struct{})
	deadlineObserved := make(chan struct{})
	var observed sync.Once
	preparations := 0
	startCalls := 0
	events := &eventLog{}
	resultCh := make(chan Result, 1)
	options := Options{
		Clock: clock,
		Stopper: stopperFunc(func(context.Context, time.Time) error {
			preparations++
			return nil
		}),
		Preflight: func(PreflightScope) error {
			close(entered)
			<-release
			return nil
		},
		Hooks: Hooks{
			Event: func(name string) {
				events.add(name)
				if name == "deadline-observed" {
					observed.Do(func() { close(deadlineObserved) })
				}
			},
			Start: func(*exec.Cmd) error {
				startCalls++
				return errors.New("unexpected provider launch")
			},
		},
	}
	go func() { resultCh <- Run(td, permit, plan, options) }()
	<-entered
	clock.timer.ch <- clock.now.Add(time.Minute)
	select {
	case <-deadlineObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("timer expiry was not observed during preflight")
	}
	close(release)
	result := <-resultCh
	require(t, errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError))
	if result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
		t.Fatalf("expired preflight was not sealed as a refusal: %+v", result)
	}
	if startCalls != 0 || preparations != 1 {
		t.Fatalf("start=%d preparations=%d, want zero starts and one preparation", startCalls, preparations)
	}
	if _, err := td.PrepareStart(0); !errors.Is(err, task.ErrAlreadyStarted) {
		t.Fatalf("expired preflight released start authority: %v", err)
	}
}

type advancingClock struct {
	mu    sync.Mutex
	now   time.Time
	timer *manualTimer
}

func newAdvancingClock() *advancingClock {
	return &advancingClock{now: time.Now(), timer: &manualTimer{ch: make(chan time.Time, 1)}}
}

func (c *advancingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *advancingClock) NewTimer(time.Duration) Timer { return c.timer }

func (c *advancingClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestClockExpiryBeforeTimerObservationCannotLaunch(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "echo")
	clock := newAdvancingClock()
	entered := make(chan struct{})
	release := make(chan struct{})
	preparations := make(chan struct{}, 1)
	requests := make(chan struct{}, 1)
	startCalls := 0
	options := Options{
		Clock: clock,
		Stopper: budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
			preparations <- struct{}{}
			return func(context.Context) error {
				requests <- struct{}{}
				return nil
			}, nil
		}),
		Preflight: func(PreflightScope) error {
			close(entered)
			<-release
			return nil
		},
		Hooks: Hooks{Start: func(*exec.Cmd) error {
			startCalls++
			return errors.New("unexpected provider launch")
		}},
	}
	resultCh := make(chan Result, 1)
	go func() { resultCh <- Run(td, permit, plan, options) }()
	<-entered
	clock.advance(time.Minute)
	close(release)
	result := <-resultCh
	require(t, errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError))
	if result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
		t.Fatalf("clock-expired preflight was not sealed as a refusal: %+v", result)
	}
	if startCalls != 0 {
		t.Fatalf("clock expiry authorized a provider launch: %d", startCalls)
	}
	select {
	case <-preparations:
	default:
		t.Fatal("clock expiry did not prepare a budget request")
	}
	select {
	case <-requests:
	default:
		t.Fatal("clock expiry did not run the prepared budget request")
	}
}

func TestPrestartRefusalIsIndependentOfStopPreparationFailure(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "echo")
	clock := newAdvancingClock()
	failure := errors.New("stop intent durability failed")
	starts, preparations := 0, 0
	result := Run(td, permit, plan, Options{
		Clock:     clock,
		Preflight: func(PreflightScope) error { clock.advance(time.Minute); return nil },
		Stopper:   budgetPreparerFunc(func(time.Time) (BudgetRequest, error) { preparations++; return nil, failure }),
		Hooks:     Hooks{Start: func(*exec.Cmd) error { starts++; return errors.New("unexpected provider launch") }},
	})
	require(t, errors.Join(result.Error, result.CleanupError, result.ReceiptError))
	if !errors.Is(result.StopError, failure) {
		t.Fatalf("stop failure was lost: %v", result.StopError)
	}
	if starts != 0 || preparations != 1 {
		t.Fatalf("starts=%d preparations=%d", starts, preparations)
	}
	if result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
		t.Fatal("definite prestart refusal lost its independent rejection")
	}
	inspection, err := td.Inspect()
	require(t, err)
	if inspection.StartedExists || !inspection.SealExists {
		t.Fatal("refusal fabricated a successful start or lost no-start evidence")
	}
	if got := string(payload(t, td, result.Outcome)); got != "start_failed: "+errBudgetExpiredBeforeStart.Error() {
		t.Fatalf("stop failure supplied terminal authority: %q", got)
	}
}
