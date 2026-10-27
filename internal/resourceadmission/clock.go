package resourceadmission

import (
	"context"
	"time"
)

// clock is the injectable time source; Now readings carry a monotonic clock.
type clock interface {
	Now() time.Time
	// Sleep waits for d or until ctx ends, returning the context error.
	Sleep(ctx context.Context, d time.Duration) error
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
