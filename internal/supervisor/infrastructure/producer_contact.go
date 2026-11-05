package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const ownerContactObservationInterval = 10 * time.Millisecond

// WaitForContact observes this accepted attempt until native contact or failure.
// It never selects another owner, starts work or renews the caller's deadline.
func (a *StartupAttempt) WaitForContact(ctx context.Context) error {
	ticker := time.NewTicker(ownerContactObservationInterval)
	defer ticker.Stop()
	return a.waitForContact(ctx, ticker.C)
}

func (a *StartupAttempt) waitForContact(ctx context.Context, ticks <-chan time.Time) error {
	done := a.NativeCompletion()
	for {
		_, contacted, err := a.Observe(ctx)
		if err != nil {
			return err
		}
		select {
		case <-done:
			return a.completedBeforeContact(ctx)
		default:
		}
		if contacted {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return a.completedBeforeContact(ctx)
		case <-ticks:
		}
	}
}

func (a *StartupAttempt) completedBeforeContact(ctx context.Context) error {
	_, _, observeErr := a.Observe(ctx)
	waitErr, _ := a.NativeWaitError()
	return errors.Join(fmt.Errorf("%w: accepted owner child exited before contact", ErrIdentity), observeErr, waitErr)
}
