package execution

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PreflightScope borrows an already-supervised lifetime. Its context cancels
// network observation on expiry; it must never be used to signal a process.
// Started native commands and their captures remain owned until they end.
// The scope expires when its synchronous callback returns and cannot be reused.
type PreflightScope struct {
	ctx       context.Context
	authorize func() error
	start     func(func() error) error
	lifetime  Lifetime
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

// StartCommand applies the task's native lifetime scope before executing the
// existing final start arbitration. Tests and non-native callbacks may keep
// using Start; every real native command uses this method.
func (s PreflightScope) StartCommand(cmd *exec.Cmd, start func() error) error {
	if cmd == nil || start == nil {
		return errBudgetExpiredBeforeStart
	}
	if s.lifetime == nil {
		return s.Start(start)
	}
	return s.Start(func() error { return s.lifetime.StartCommand(cmd, start) })
}

// FinishCommand lets the guardian drain residual descendants after the owned
// Wait and before capture waits for EOF. It is idempotent for a command whose
// scope was already stopped by the deadline owner.
func (s PreflightScope) FinishCommand(cmd *exec.Cmd) error {
	if cmd == nil {
		return task.ErrIdentityMismatch
	}
	if s.lifetime == nil {
		return nil
	}
	return s.lifetime.FinishCommand(context.WithoutCancel(s.ctx), cmd)
}

// deadlineContext exposes the timer owner's deadline. Done comes from a derived
// cancel context driven by that owner's single expiry observer.
type deadlineContext struct {
	context.Context
	deadline time.Time
}

func (c deadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func (b *budgetOwner) runScoped(opts Options, work func(PreflightScope) error) error {
	_, err := b.runScopedPlan(opts, Plan{}, func(scope PreflightScope, _ Plan) (Plan, error) {
		return Plan{}, work(scope)
	})
	return err
}

func (b *budgetOwner) runScopedPlan(opts Options, plan Plan, work func(PreflightScope, Plan) (Plan, error)) (Plan, error) {
	ctx, cancel := context.WithCancel(b.workContext)
	defer cancel()
	scope := PreflightScope{
		ctx:       ctx,
		authorize: func() error { return b.authorize(opts, "preflight-authorized") },
		start:     func(start func() error) error { return b.authorizeAndStart(opts, start) },
		lifetime:  opts.Lifetime,
	}
	if err := scope.Authorize(); err != nil {
		return Plan{}, err
	}
	return work(scope, plan)
}

// SupervisedOptions supplies the same clock and durable stop boundary used by
// ordinary execution. There is no process, provider, or publication capability.
type SupervisedOptions struct {
	Clock    Clock
	Stopper  Stopper
	Lifetime Lifetime
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
	executionOptions := Options{Clock: opts.Clock, Stopper: opts.Stopper, Lifetime: opts.Lifetime}
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
	stopErr = budget.await()
	if opts.Lifetime != nil {
		workErr = errors.Join(workErr, opts.Lifetime.CloseTask(context.Background()))
	}
	return workErr, stopErr
}
