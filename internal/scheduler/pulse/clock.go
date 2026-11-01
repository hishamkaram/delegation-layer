package pulse

import (
	"time"

	"github.com/jonboulle/clockwork"
)

// intervalClock owns a single anchor and derives its synthetic wall time from
// monotonic elapsed time. Since and Until must use that same domain: GoCron's
// location conversion strips monotonic readings from its appointment values.
// Embedded duration timers use native monotonic waits, independent of wall
// changes. Their channel timestamps are not calendar inputs to GoCron.
type intervalClock struct {
	clockwork.Clock
	anchor  time.Time
	elapsed func(time.Time) time.Duration
}

func newIntervalClock() intervalClock {
	return intervalClock{Clock: clockwork.NewRealClock(), anchor: time.Now(), elapsed: time.Since}
}

func (c intervalClock) Now() time.Time { return c.anchor.Add(c.elapsed(c.anchor)) }

func (c intervalClock) Since(instant time.Time) time.Duration { return c.Now().Sub(instant) }

func (c intervalClock) Until(instant time.Time) time.Duration { return instant.Sub(c.Now()) }
