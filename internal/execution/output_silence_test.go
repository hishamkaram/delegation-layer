package execution

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type silenceTestTimer struct {
	deadline time.Time
	ch       chan time.Time
	stopped  bool
	clock    *silenceTestClock
}

func (t *silenceTestTimer) C() <-chan time.Time { return t.ch }
func (t *silenceTestTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	if was {
		t.clock.stopped <- t.deadline
	}
	return was
}

type silenceTestClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*silenceTestTimer
	created chan time.Time
	stopped chan time.Time
}

func newSilenceTestClock() *silenceTestClock {
	return &silenceTestClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), created: make(chan time.Time, 32), stopped: make(chan time.Time, 32)}
}
func (c *silenceTestClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *silenceTestClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &silenceTestTimer{deadline: c.now.Add(d), clock: c, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	c.created <- t.deadline
	return t
}

func (c *silenceTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		if !t.stopped && !c.now.Before(t.deadline) {
			t.stopped = true
			t.ch <- c.now
		}
	}
}

func awaitSilenceTimer(t *testing.T, c *silenceTestClock) time.Time {
	t.Helper()
	select {
	case d := <-c.created:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("timer was not armed")
		return time.Time{}
	}
}

type silenceTestStopper struct {
	prepare func(time.Time, string) (BudgetRequest, error)
}

func (s silenceTestStopper) PrepareBudget(d time.Time) (BudgetRequest, error) {
	return s.prepare(d, "budget")
}

func (s silenceTestStopper) PrepareStop(d time.Time, c string) (BudgetRequest, error) {
	return s.prepare(d, c)
}

func TestOutputSilenceActivityExtendsOnlySilenceDeadline(t *testing.T) {
	c := newSilenceTestClock()
	selected := make(chan string, 1)
	opts := Options{Stopper: silenceTestStopper{prepare: func(_ time.Time, cause string) (BudgetRequest, error) {
		selected <- cause
		return func(context.Context) error { return nil }, nil
	}}}
	b, _ := armBudget(c, time.Minute, opts)
	t.Cleanup(func() { b.complete(opts); <-b.done })
	wall := awaitSilenceTimer(t, c)
	b.configureOutputSilence(10 * time.Second)
	b.providerStarted()
	if got := awaitSilenceTimer(t, c); !got.Equal(wall.Add(-50 * time.Second)) {
		t.Fatalf("initial silence=%s wall=%s", got, wall)
	}
	c.advance(5 * time.Second)
	b.outputActivity()
	if got := awaitSilenceTimer(t, c); !got.Equal(wall.Add(-45 * time.Second)) {
		t.Fatalf("activity silence=%s", got)
	}
	c.advance(10 * time.Second)
	select {
	case cause := <-selected:
		if cause != task.StopCauseOutputSilence {
			t.Fatalf("cause=%s", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("silence did not expire")
	}
	b.complete(opts)
	if err := b.await(); err != nil {
		t.Fatal(err)
	}
}

func TestProviderWaitDisarmsSilenceBeforeCaptureCompletion(t *testing.T) {
	c := newSilenceTestClock()
	called := make(chan string, 1)
	opts := Options{Stopper: silenceTestStopper{prepare: func(_ time.Time, cause string) (BudgetRequest, error) { called <- cause; return nil, nil }}}
	b, _ := armBudget(c, time.Minute, opts)
	t.Cleanup(func() { b.complete(opts); <-b.done })
	awaitSilenceTimer(t, c)
	b.configureOutputSilence(time.Second)
	b.providerStarted()
	awaitSilenceTimer(t, c)
	b.providerWaited()
	select {
	case <-c.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("silence timer was not disarmed after provider Wait")
	}
	c.advance(2 * time.Second)
	b.complete(opts)
	if err := b.await(); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-called:
		t.Fatalf("stopped exited provider: %s", cause)
	default:
	}
}

func TestSilencePreparationFailureCannotRunReturnedOperation(t *testing.T) {
	c := newSilenceTestClock()
	failure := errors.New("stop persistence failed")
	ran := false
	opts := Options{Stopper: silenceTestStopper{prepare: func(time.Time, string) (BudgetRequest, error) {
		return func(context.Context) error { ran = true; return nil }, failure
	}}}
	b, _ := armBudget(c, time.Minute, opts)
	t.Cleanup(func() { b.complete(opts); <-b.done })
	awaitSilenceTimer(t, c)
	b.configureOutputSilence(time.Second)
	b.providerStarted()
	awaitSilenceTimer(t, c)
	c.advance(time.Second)
	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
		t.Fatal("failed preparation did not settle")
	}
	b.complete(opts)
	if !errors.Is(b.await(), failure) || ran {
		t.Fatalf("failure=%v ran=%t", b.await(), ran)
	}
}

func TestCaptureActivityCountsPartialBytesBeforeWriterFailure(t *testing.T) {
	activity := 0
	r := activityReader{Reader: strings.NewReader("partial"), activity: func() { activity++ }}
	var buffer [3]byte
	n, err := r.Read(buffer[:])
	if n != 3 || err != nil || activity != 1 {
		t.Fatalf("read=%d %v activity=%d", n, err, activity)
	}
}

func TestNullOutputCommandDoesNotInstallCapturePipes(t *testing.T) {
	cmd := &exec.Cmd{Stdout: io.Discard, Stderr: io.Discard}
	starts, waits, finishes := 0, 0, 0
	scope := commandTestScope{ctx: commandTestContext(t), finish: func(*exec.Cmd) error { finishes++; return nil }}
	err := RunScopedCommand(scope, cmd, CommandHooks{NullOutput: true, Start: func(c *exec.Cmd) error {
		starts++
		if c.Stdout != nil || c.Stderr != nil {
			t.Error("capture pipes installed")
		}
		return nil
	}, Wait: func(*exec.Cmd) error { waits++; return nil }})
	if err != nil || starts != 1 || waits != 1 || finishes != 1 {
		t.Fatalf("result=%v start/wait/finish=%d/%d/%d", err, starts, waits, finishes)
	}
}

func TestOutputSilenceDelayedTimerCannotAuthorizeReplyOrRenewWindow(t *testing.T) {
	c := newSilenceTestClock()
	selected := make(chan string, 1)
	opts := Options{Stopper: silenceTestStopper{prepare: func(_ time.Time, cause string) (BudgetRequest, error) { selected <- cause; return nil, nil }}}
	b, _ := armBudget(c, time.Minute, opts)
	t.Cleanup(func() { b.complete(opts); <-b.done })
	awaitSilenceTimer(t, c)
	b.configureOutputSilence(time.Second)
	b.providerStarted()
	awaitSilenceTimer(t, c)
	// Move the injected monotonic clock without delivering either timer.
	c.mu.Lock()
	c.now = c.now.Add(2 * time.Second)
	c.mu.Unlock()
	b.outputActivity()
	if err := b.authorize(opts, "interaction-reply"); err == nil {
		t.Fatal("overdue silence authorized a reply")
	}
	if cause := <-selected; cause != task.StopCauseOutputSilence {
		t.Fatalf("cause=%q", cause)
	}
}
