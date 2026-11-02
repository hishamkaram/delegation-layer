package pulse

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
)

type shiftedWall struct {
	clockwork.Clock
	shift atomic.Int64
}

func (c *shiftedWall) Now() time.Time { return c.Clock.Now().Add(time.Duration(c.shift.Load())) }

func (c *shiftedWall) Since(instant time.Time) time.Duration { return c.Now().Sub(instant) }

func (c *shiftedWall) Until(instant time.Time) time.Duration { return instant.Sub(c.Now()) }

type recordedTimer struct {
	clockwork.Timer
	duration time.Duration
	stopped  chan struct{}
	once     sync.Once
}

func (t *recordedTimer) Stop() bool {
	stopped := t.Timer.Stop()
	t.once.Do(func() { close(t.stopped) })
	return stopped
}

type recordingClock struct {
	clockwork.Clock
	armed chan *recordedTimer
}

func (c *recordingClock) AfterFunc(duration time.Duration, callback func()) clockwork.Timer {
	timer := &recordedTimer{
		Timer: c.Clock.AfterFunc(duration, callback), duration: duration, stopped: make(chan struct{}),
	}
	c.armed <- timer
	return timer
}

// completed observes GoCron after the source callback, even when its wake
// buffer is full. The optional gate proves native shutdown joins an active run.
type completionMonitor struct {
	completed chan struct{}
	entered   chan struct{}
	release   chan struct{}
}

func (m *completionMonitor) IncrementJob(_ uuid.UUID, _ string, _ []string, status gocron.JobStatus) {
	if status == gocron.Success {
		if m.entered != nil {
			m.entered <- struct{}{}
			<-m.release
		}
		m.completed <- struct{}{}
	}
}

func (*completionMonitor) RecordJobTiming(time.Time, time.Time, uuid.UUID, string, []string) {}

type sourceFixture struct {
	source    *Source
	scheduler gocron.Scheduler
	monotonic *clockwork.FakeClock
	wall      *shiftedWall
	clock     intervalClock
	armed     <-chan *recordedTimer
	monitor   *completionMonitor
}

func producerFixture(t *testing.T, monitor *completionMonitor) sourceFixture {
	t.Helper()
	anchor := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	monotonic := clockwork.NewFakeClockAt(anchor)
	wall := &shiftedWall{Clock: monotonic}
	recorded := &recordingClock{Clock: wall, armed: make(chan *recordedTimer, 32)}
	clock := intervalClock{Clock: recorded, anchor: anchor, elapsed: monotonic.Since}
	scheduler, err := gocron.NewScheduler(gocron.WithClock(clock), gocron.WithMonitor(monitor))
	if err != nil {
		t.Fatal(err)
	}
	source, err := newSource(scheduler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	return sourceFixture{
		source: source, scheduler: scheduler, monotonic: monotonic, wall: wall,
		clock: clock, armed: recorded.armed, monitor: monitor,
	}
}

func (f sourceFixture) step(t *testing.T) {
	t.Helper()
	timer := await(t, f.armed)
	if timer.duration != time.Second {
		t.Fatalf("next pulse delay=%s, want one second", timer.duration)
	}
	f.monotonic.Advance(time.Second)
	f.awaitCompleted(t)
}

func (f sourceFixture) awaitCompleted(t *testing.T) {
	t.Helper()
	await(t, f.monitor.completed)
	// The callback can finish while GoCron's accepted completion handler still
	// prunes schedules. Cross its owner loop before advancing the fake clock.
	if jobs := f.scheduler.Jobs(); len(jobs) != 1 {
		t.Fatalf("producer has %d jobs after completion, want one interval job", len(jobs))
	}
}

func await[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pulse ownership gate")
	}
	var zero T
	return zero
}

func waitContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

type observedContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}
