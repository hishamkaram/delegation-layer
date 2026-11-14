package execution

import (
	"context"
	"errors"
	"fmt"
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

type silenceRunStop struct {
	deadline time.Time
	cause    string
}

// The fixture owns hook gates and Run's finite child/capture join. Its result is
// read only after done closes; cleanup releases every gate before joining Run.
type silenceRunFixture struct {
	clock                             *silenceTestClock
	events                            eventLog
	started, readOutput, finish, done chan struct{}
	captured                          chan error
	stopped                           chan silenceRunStop
	readOnce, finishOnce              sync.Once
	result                            Result
}

func newSilenceRunFixture(t *testing.T, beforeRelease func()) *silenceRunFixture {
	t.Helper()
	td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", func(meta *task.MetaRecord, _ *Plan) {
		meta.RequestedConfig.OutputSilenceTimeout = "10s"
	})
	f := &silenceRunFixture{
		clock: newSilenceTestClock(), started: make(chan struct{}), readOutput: make(chan struct{}),
		finish: make(chan struct{}), done: make(chan struct{}), captured: make(chan error, 1), stopped: make(chan silenceRunStop, 1),
	}
	opts := Options{Clock: f.clock, Stopper: silenceTestStopper{prepare: func(deadline time.Time, cause string) (BudgetRequest, error) {
		return func(context.Context) error {
			if beforeRelease != nil {
				beforeRelease()
			}
			f.release()
			f.stopped <- silenceRunStop{deadline, cause}
			return nil
		}, nil
	}}, Hooks: Hooks{
		Event: func(event string) {
			switch event {
			case "started-receipt":
				f.events.add(event)
				close(f.started)
			case "parent-fds-closed", "wait-completed", "stdout-raw-closed", "stderr-raw-closed", "completion-observed", "output-silence-observed", "sealed", "published":
				f.events.add(event)
			}
		},
		Wait: func(cmd *exec.Cmd) error {
			<-f.finish
			return cmd.Wait()
		},
		Capture: f.capture,
	}}
	t.Cleanup(func() {
		f.release()
		if _, err := f.join(); err != nil {
			t.Error(err)
		}
	})
	go func() { f.result = Run(td, permit, plan, opts); close(f.done) }()
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not record an actual provider start")
	}
	return f
}

func (f *silenceRunFixture) releaseRead() {
	f.readOnce.Do(func() { close(f.readOutput) })
}

func (f *silenceRunFixture) release() {
	f.releaseRead()
	f.finishOnce.Do(func() { close(f.finish) })
}

func (f *silenceRunFixture) join() (Result, error) {
	select {
	case <-f.done:
		return f.result, nil
	case <-time.After(5 * time.Second):
		return Result{}, fmt.Errorf("Run did not complete within 5s: phases=%v gates(readOutput=%s finish=%s done=%s)", f.events.snapshot(), silenceGateState(f.readOutput), silenceGateState(f.finish), silenceGateState(f.done))
	}
}

func silenceGateState(gate <-chan struct{}) string {
	select {
	case <-gate:
		return "closed"
	default:
		return "open"
	}
}

func (f *silenceRunFixture) capture(name string, dst io.Writer, src io.Reader) error {
	if name != "stdout" {
		_, err := io.Copy(dst, src)
		return err
	}
	<-f.readOutput
	_, err := io.CopyN(dst, src, 1)
	f.captured <- err
	if err != nil {
		return err
	}
	<-f.finish
	_, err = io.Copy(dst, src)
	return err
}

func TestRunCaptureActivityExtendsOutputSilenceDeadline(t *testing.T) {
	f := newSilenceRunFixture(t, nil)
	start := f.clock.Now()
	if wall := awaitSilenceTimer(t, f.clock); !wall.Equal(start.Add(time.Minute)) {
		t.Fatalf("wall deadline=%s, want %s", wall, start.Add(time.Minute))
	}
	if initial := awaitSilenceTimer(t, f.clock); !initial.Equal(start.Add(10 * time.Second)) {
		t.Fatalf("initial silence deadline=%s, want %s", initial, start.Add(10*time.Second))
	}
	f.clock.advance(5 * time.Second)
	f.releaseRead()
	select {
	case err := <-f.captured:
		require(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run capture did not read the released provider byte")
	}
	if extended := awaitSilenceTimer(t, f.clock); !extended.Equal(start.Add(15 * time.Second)) {
		t.Fatalf("captured output silence deadline=%s, want %s", extended, start.Add(15*time.Second))
	}
	f.clock.advance(5 * time.Second)
	select {
	case stop := <-f.stopped:
		t.Fatalf("Run stopped at the original deadline after later output: %+v", stop)
	default:
	}
	f.clock.advance(5 * time.Second)
	select {
	case stop := <-f.stopped:
		if stop.cause != task.StopCauseOutputSilence || !stop.deadline.Equal(start.Add(15*time.Second)) {
			t.Fatalf("Run selected the wrong stop after captured output: %+v", stop)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop at the extended silence deadline")
	}
	result, err := f.join()
	require(t, err)
	if result.Outcome == nil || result.StopError != nil {
		t.Fatalf("Run did not publish and join its stop request: %+v", result)
	}
}

func TestSilenceRunStopAcknowledgesReleasedFixtureGates(t *testing.T) {
	beforeRelease := make(chan struct{})
	allowRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowRelease) }) }
	f := newSilenceRunFixture(t, func() {
		close(beforeRelease)
		<-allowRelease
	})
	t.Cleanup(release)
	start := f.clock.Now()
	awaitSilenceTimer(t, f.clock)
	awaitSilenceTimer(t, f.clock)
	f.clock.advance(10 * time.Second)
	select {
	case <-beforeRelease:
	case <-time.After(5 * time.Second):
		t.Fatal("stop callback did not reach the fixture release gate")
	}
	select {
	case stop := <-f.stopped:
		t.Fatalf("stop acknowledged before fixture gates were released: %+v", stop)
	default:
	}
	release()
	select {
	case stop := <-f.stopped:
		if stop.cause != task.StopCauseOutputSilence || !stop.deadline.Equal(start.Add(10*time.Second)) {
			t.Fatalf("Run selected the wrong stop: %+v", stop)
		}
		if read, finish := silenceGateState(f.readOutput), silenceGateState(f.finish); read != "closed" || finish != "closed" {
			t.Fatalf("stop acknowledged with fixture gates readOutput=%s finish=%s", read, finish)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop callback did not acknowledge released fixture gates")
	}
	result, err := f.join()
	require(t, err)
	if result.Outcome == nil || result.StopError != nil {
		t.Fatalf("Run did not publish and join its stop request: %+v", result)
	}
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
