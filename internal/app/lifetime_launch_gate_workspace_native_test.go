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
func (s worktreeGateTestScope) Run(command *exec.Cmd, hooks execution.CommandHooks) (resultErr error) {
	id, err := randomid.NewTaskID()
	if err != nil {
		return err
	}
	plan, err := lifetime.Prepare(id, "worktree-lease-test", id)
	if err != nil {
		return err
	}
	native, err := lifetime.CreatePrepared(plan)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := native.DestroyEmpty(); cleanupErr != nil {
			resultErr = errors.Join(resultErr, cleanupErr)
			return
		}
		resultErr = errors.Join(resultErr, native.Release())
	}()
	if err = native.BindOwnerProcess(); err != nil {
		return err
	}
	fixture := &worktreeGateTestLifetime{configuration: s, native: native}
	deadline, finite := s.ctx.Deadline()
	if !finite {
		return errors.New("native worktree fixture requires a finite deadline")
	}
	workErr, stopErr := execution.RunSupervised(deadline, execution.SupervisedOptions{Stopper: fixture, Lifetime: fixture}, func(scope execution.PreflightScope) error {
		return execution.RunScopedCommand(scope, command, hooks)
	})
	return errors.Join(workErr, stopErr)
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
	gate, err := guardian.NewLaunchGateFactory(f.configuration.runner)(command)
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
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return f.native.Terminate(cleanupContext, nil)
}

func (f *worktreeGateTestLifetime) FinishCommand(ctx context.Context, _ *exec.Cmd) error {
	return f.Terminate(ctx)
}

func (f *worktreeGateTestLifetime) CloseTask(ctx context.Context) error {
	return f.Terminate(ctx)
}
