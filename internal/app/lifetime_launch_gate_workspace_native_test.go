package app

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"

	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

// The fixture borrows production budget, execution, launch-gate, and native
// scope primitives. It owns no alternative Start/Wait, timer, or signal loop.
func (s worktreeGateTestScope) Run(command *exec.Cmd, hooks execution.CommandHooks) error {
	return s.RunWork(func(scope execution.PreflightScope) error {
		return execution.RunScopedCommand(scope, command, hooks)
	})
}

// RunWork borrows one finite budget for a complete worker operation. Each
// command gets its own native scope, as Run did before operations were grouped:
// a terminated cgroup is not a reusable admission boundary.
func (s worktreeGateTestScope) RunWork(work func(execution.PreflightScope) error) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	fixture := &worktreeGateTestLifetime{configuration: s}
	deadline, finite := s.ctx.Deadline()
	if !finite {
		return errors.New("native worktree fixture requires a finite deadline")
	}
	workErr, stopErr := execution.RunSupervisedContext(s.ctx, deadline, execution.SupervisedOptions{Stopper: fixture, Lifetime: fixture}, work)
	return errors.Join(workErr, stopErr)
}

func newWorktreeGateNativeScope() (*lifetime.Scope, error) {
	id, err := randomid.NewTaskID()
	if err != nil {
		return nil, err
	}
	plan, err := lifetime.Prepare(id, "worktree-lease-test", id)
	if err != nil {
		return nil, err
	}
	native, err := lifetime.CreatePrepared(plan)
	if err != nil {
		return nil, err
	}
	if bindErr := native.BindOwnerProcess(); bindErr != nil {
		if cleanupErr := native.DestroyEmpty(); cleanupErr != nil {
			return nil, errors.Join(bindErr, cleanupErr)
		}
		return nil, errors.Join(bindErr, native.Release())
	}
	return native, nil
}

type worktreeGateTestLifetime struct {
	configuration worktreeGateTestScope
	native        *lifetime.Scope
	mu            sync.Mutex
	stopped       bool
}

func (f *worktreeGateTestLifetime) StartCommand(command *exec.Cmd, start func() error) error {
	// This primitive fixture serializes launch with stop. Production uses the
	// guardian protocol; this test does not prove progress through stalled Start.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return errors.New("native worktree fixture already stopped")
	}
	if f.native != nil {
		return errors.New("native worktree fixture still owns a previous command")
	}
	native, err := newWorktreeGateNativeScope()
	if err != nil {
		return err
	}
	f.native = native
	gate, err := guardian.NewLaunchGateFactory(f.configuration.runner)(command, nil)
	if err != nil {
		return err
	}
	if err = f.native.Configure(command); err != nil {
		return errors.Join(err, gate.Abort())
	}
	startErr := start()
	if err = errors.Join(startErr, gate.Started(startErr)); err != nil {
		return errors.Join(err, gate.Abort())
	}
	if _, err = f.native.ObserveStarted(command.Process.Pid); err != nil {
		return errors.Join(err, gate.Abort())
	}
	if f.configuration.abort {
		return gate.Abort()
	}
	return gate.Release()
}

func (f *worktreeGateTestLifetime) PrepareBudget(time.Time) (execution.BudgetRequest, error) {
	if f.configuration.facts != nil {
		f.configuration.facts.budgetRequested.Store(true)
	}
	return func(ctx context.Context) error {
		err := f.Terminate(ctx)
		if err == nil && f.configuration.facts != nil {
			f.configuration.facts.budgetCompleted.Store(true)
		}
		return err
	}, nil
}

func (f *worktreeGateTestLifetime) Terminate(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	if f.native == nil {
		return nil
	}
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return f.native.Terminate(cleanupContext, nil)
}

func (f *worktreeGateTestLifetime) FinishCommand(ctx context.Context, _ *exec.Cmd) error {
	// Execution has joined Wait. Drain and dispose this command's scope before
	// permitting the operation's next command; budget expiry still revokes it.
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeNative(ctx)
}

// closeNative runs with mu held. Failed emptiness proof retains the scope.
func (f *worktreeGateTestLifetime) closeNative(ctx context.Context) error {
	if f.native == nil {
		return nil
	}
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	stopErr := f.native.Terminate(cleanupContext, nil)
	if cleanupErr := f.native.DestroyEmpty(); cleanupErr != nil {
		return errors.Join(stopErr, cleanupErr)
	}
	if releaseErr := f.native.Release(); releaseErr != nil {
		return errors.Join(stopErr, releaseErr)
	}
	f.native = nil
	return stopErr
}

func (f *worktreeGateTestLifetime) CloseTask(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return f.closeNative(ctx)
}
