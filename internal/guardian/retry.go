package guardian

import (
	"context"
	"time"
)

func waitLifetimeWorkerRecordRetry(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	timer := time.NewTimer(defaultLifetimeWorkerWaitRetry)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func resolveLifetimeWorkerRecordRetry(configured func(context.Context) error) func(context.Context) error {
	if configured != nil {
		return configured
	}
	return waitLifetimeWorkerRecordRetry
}

func waitLifetimeFinalizationBackoff(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		return context.Canceled
	}
	if delay <= 0 {
		delay = defaultLifetimeFinalizationBackoff
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func resolveLifetimeFinalizationBackoff(configured func(context.Context, time.Duration) error) func(context.Context, time.Duration) error {
	if configured != nil {
		return configured
	}
	return waitLifetimeFinalizationBackoff
}

func nextLifetimeFinalizationBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return defaultLifetimeFinalizationBackoff
	}
	if current >= maxLifetimeFinalizationBackoff/2 {
		return maxLifetimeFinalizationBackoff
	}
	return current * 2
}

// waitFinalizationBackoff treats an injected wait failure as a test-hook
// failure, never as permission to abandon an owned scope. The production
// default is already bounded; the fallback also keeps a faulty test hook from
// turning a retry loop into a busy loop.
func (g *Guardian) waitFinalizationBackoff(ctx context.Context, delay time.Duration) {
	if err := g.options.FinalizationBackoff(ctx, delay); err == nil {
		return
	}
	waitResidentLifetimeFinalizationBackoff(delay)
}

func (g *Guardian) waitFinalizationRetry(ctx context.Context, delay time.Duration, retry func(context.Context) error) {
	if retry != nil && retry(ctx) != nil {
		waitResidentLifetimeFinalizationBackoff(delay)
		return
	}
	g.waitFinalizationBackoff(ctx, delay)
}

// waitResidentLifetimeFinalizationBackoff is the non-failing fallback for an
// injected retry wait. Finalization cannot return merely because its wait hook
// failed, and the bounded delay prevents a faulty hook from creating a busy
// loop while the guardian still owns native authority.
func waitResidentLifetimeFinalizationBackoff(delay time.Duration) {
	if delay <= 0 {
		delay = defaultLifetimeFinalizationBackoff
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	<-timer.C
}
