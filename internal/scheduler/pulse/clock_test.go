package pulse

import (
	"testing"
	"time"
)

func TestSourceIntervalClockSurvivesWallJumpsAndSuspend(t *testing.T) {
	f := producerFixture(t, &completionMonitor{completed: make(chan struct{}, 16)})
	f.source.Start()
	f.step(t)
	if err := f.source.Wait(waitContext(t)); err != nil {
		t.Fatal(err)
	}
	for _, shift := range []time.Duration{-time.Hour, time.Hour, 24 * time.Hour} {
		before := f.clock.Now()
		f.wall.shift.Store(int64(shift))
		// Simulate suspend with a stopped interval clock and advancing wall time.
		// Native OS sleep/resume behavior still requires a native acceptance run.
		for range 3 {
			f.step(t)
			if err := f.source.Wait(waitContext(t)); err != nil {
				t.Fatal(err)
			}
		}
		if got := f.clock.Now().Sub(before); got != 3*time.Second {
			t.Fatalf("wall shift %s: elapsed=%s, want three pulse intervals", shift, got)
		}
		// UTC removes the monotonic reading, matching GoCron's location conversion.
		past := before.UTC()
		future := f.clock.Now().Add(time.Second).UTC()
		if got := f.clock.Since(past); got != 3*time.Second {
			t.Fatalf("wall shift %s: Since=%s", shift, got)
		}
		if got := f.clock.Until(future); got != time.Second {
			t.Fatalf("wall shift %s: Until=%s", shift, got)
		}
	}
}

func TestSourceResumesAfterElapsedClockJump(t *testing.T) {
	f := producerFixture(t, &completionMonitor{completed: make(chan struct{}, 8)})
	f.source.Start()
	f.step(t)
	if err := f.source.Wait(waitContext(t)); err != nil {
		t.Fatal(err)
	}
	await(t, f.armed)
	// Also model hosts whose elapsed clock includes suspension. One late timer
	// fires, then production resumes at the next interval without a wake backlog.
	f.monotonic.Advance(90*time.Second + 500*time.Millisecond)
	f.awaitCompleted(t)
	next := await(t, f.armed)
	if next.duration != 500*time.Millisecond {
		t.Fatalf("resume delay=%s, want the next future half-second", next.duration)
	}
	if err := f.source.Wait(waitContext(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.source.wake:
		t.Fatal("elapsed clock jump accumulated a pulse backlog")
	default:
	}
	f.monotonic.Advance(500 * time.Millisecond)
	f.awaitCompleted(t)
	if err := f.source.Wait(waitContext(t)); err != nil {
		t.Fatal(err)
	}
	f.step(t)
	if err := f.source.Wait(waitContext(t)); err != nil {
		t.Fatal(err)
	}
}

func TestSourceIntervalClockTimerUsesSameElapsedDomain(t *testing.T) {
	f := producerFixture(t, &completionMonitor{completed: make(chan struct{}, 1)})
	started := f.clock.Now().UTC()
	timer := f.clock.NewTimer(time.Second)
	defer timer.Stop()
	f.wall.shift.Store(int64(-24 * time.Hour))
	f.monotonic.Advance(time.Second)
	await(t, timer.Chan())
	if elapsed := f.clock.Since(started); elapsed != time.Second {
		t.Fatalf("timer fired in a different interval domain: Since=%s", elapsed)
	}
}
