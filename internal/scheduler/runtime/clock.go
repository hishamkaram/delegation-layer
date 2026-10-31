package runtime

import (
	"context"
	"time"
)

type nativeClock struct{}

func (nativeClock) Now() time.Time { return time.Now().UTC() }

func (nativeClock) Wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
