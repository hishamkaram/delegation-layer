//go:build linux || darwin

package resourceadmission

import (
	"context"
	"time"
)

// tickRecorder wraps the real native observer and the checker's clock. Every
// read is forwarded unchanged; it only remembers each tick and the time the
// checker stamped on it, and lets a native test schedule real load between
// ticks. It never supplies telemetry. Pass it as both observer and clock.
type tickRecorder struct {
	observer
	// base is the clock behind the recorder; nil is the real system clock.
	base   clock
	snaps  []snapshot
	stamps []time.Time
	after  func(tick int)
	// stampNext is set after a tick: the checker's next Now reading is the
	// time it assigns to that tick, after observe and the after hook return.
	stampNext bool
}

func (r *tickRecorder) observe(ctx context.Context, target *Target) snapshot {
	snap := r.observer.observe(ctx, target)
	r.snaps = append(r.snaps, snap)
	if r.after != nil {
		r.after(len(r.snaps) - 1)
	}
	r.stampNext = true
	return snap
}

func (r *tickRecorder) source() clock {
	if r.base == nil {
		return systemClock{}
	}
	return r.base
}

func (r *tickRecorder) Now() time.Time {
	now := r.source().Now()
	if r.stampNext {
		r.stamps = append(r.stamps, now)
		r.stampNext = false
	}
	return now
}

func (r *tickRecorder) Sleep(ctx context.Context, d time.Duration) error {
	return r.source().Sleep(ctx, d)
}

// intervals returns the elapsed time of each consecutive pair of ticks, as
// the checker measured it. Interval i runs from snaps[i] to snaps[i+1].
func (r *tickRecorder) intervals() []time.Duration {
	var elapsed []time.Duration
	for i := 1; i < min(len(r.stamps), len(r.snaps)); i++ {
		elapsed = append(elapsed, r.stamps[i].Sub(r.stamps[i-1]))
	}
	return elapsed
}
