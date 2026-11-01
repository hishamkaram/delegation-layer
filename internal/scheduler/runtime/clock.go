package runtime

import (
	"context"
	"time"
)

type nativeClock struct{}

func (nativeClock) Now() time.Time { return time.Now().UTC() }

// timerWaker preserves standalone Run callers without starting a shared source.
type timerWaker struct{}

func (timerWaker) Wait(ctx context.Context) error {
	timer := time.NewTimer(IdleInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
