package execution

import (
	"context"
	"errors"
	"time"
)

// PreflightScope borrows an already-supervised lifetime. Its context cancels
// network observation on expiry; it must never be used to signal a process.
// Started native commands and their captures remain owned until they end.
// The scope expires when its synchronous callback returns and cannot be reused.
type PreflightScope struct {
	ctx       context.Context
	authorize func() error
	start     func(func() error) error
}

// Context carries the existing owner's absolute deadline, without another timer.
func (s PreflightScope) Context() context.Context { return s.ctx }

// Authorize checks the same deadline/start arbitration as the eventual provider
// start. It is only a timing gate, not a submission or process-start permit.
func (s PreflightScope) Authorize() error {
	if s.ctx == nil || s.authorize == nil || s.ctx.Err() != nil {
		return errBudgetExpiredBeforeStart
	}
	return s.authorize()
}

// Start performs the final deadline arbitration and reserves exactly one owned
// native command start. The start callback runs without the budget mutex so a
// blocked platform start cannot suppress deadline observation or supervisor
// stopping.
func (s PreflightScope) Start(start func() error) error {
	if s.ctx == nil || s.start == nil || start == nil || s.ctx.Err() != nil {
		return errBudgetExpiredBeforeStart
	}
	return s.start(start)
}

// deadlineContext exposes the timer owner's deadline. Done comes from a derived
// cancel context driven by that owner's single expiry observer.
type deadlineContext struct {
	context.Context
	deadline time.Time
}

func (c deadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func (b *budgetOwner) runScoped(opts Options, work func(PreflightScope) error) error {
	ctx, cancel := context.WithCancel(b.workContext)
	defer cancel()
	scope := PreflightScope{
		ctx:       ctx,
		authorize: func() error { return b.authorize(opts, "preflight-authorized") },
		start:     func(start func() error) error { return b.authorizeAndStart(opts, start) },
	}
	if err := scope.Authorize(); err != nil {
		return err
	}
	return work(scope)
}

// SupervisedOptions supplies the same clock and durable stop boundary used by
// ordinary execution. There is no process, provider, or publication capability.
type SupervisedOptions struct {
	Clock   Clock
	Stopper Stopper
}

// RunSupervised runs finite inspection work inside an existing supervisor job.
// Deadline is persisted before queueing; it is never renewed on worker startup.
// The caller validates and consumes its worker permit before entering here.
// This function reuses the ordinary budget owner and retains its stop observer
// after work returns. Work must join every owned command and capture itself.
func RunSupervised(deadline time.Time, opts SupervisedOptions, work func(PreflightScope) error) (workErr, stopErr error) {
	if deadline.IsZero() || work == nil || opts.Stopper == nil {
		return errors.New("invalid supervised inspection lifetime"), nil
	}
	executionOptions := Options{Clock: opts.Clock, Stopper: opts.Stopper}
	budget := armBudgetUntil(executionOptions.clock(), deadline, executionOptions)
	workErr = budget.runScoped(executionOptions, work)
	if workErr == nil {
		workErr = budget.authorize(executionOptions, "inspection-completed-before-deadline")
	}
	budget.complete(executionOptions)
	budget.mu.Lock()
	expired := budget.expired
	budget.mu.Unlock()
	if expired {
		workErr = errors.Join(workErr, errBudgetExpiredBeforeStart)
	}
	return workErr, budget.await()
}
