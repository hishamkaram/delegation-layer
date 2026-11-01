package execution

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

// This adapter belongs to the dedicated owner process only. Execution owns
// streams/Wait; lifetime.Scope owns native identity, signals and empty proof.
type nativeCommandLifetime struct {
	native   *lifetime.Scope
	mu       sync.Mutex
	stopped  bool
	pending  chan struct{}
	launches []lifetime.LaunchRef
}

func newNativeCommandLifetime(ctx context.Context, id string) (*nativeCommandLifetime, error) {
	// This passive, unjournaled probe uses the existing cooperative backend
	// inside its own process. Task/guardian backend selection is unchanged.
	plan, err := lifetime.NewPlan(lifetime.KindProcessGroup, lifetime.AssuranceCooperative, "pg:"+id)
	if err != nil {
		return nil, err
	}
	native, err := lifetime.CreatePreparedContext(ctx, plan)
	if err != nil {
		return nil, err
	}
	if err := native.BindOwnerProcess(); err != nil {
		return nil, errors.Join(err, native.DestroyEmpty(), native.Release())
	}
	return &nativeCommandLifetime{native: native}, nil
}

func (owner *nativeCommandLifetime) StartCommand(command *exec.Cmd, start func() error) error {
	owner.mu.Lock()
	if owner.stopped {
		owner.mu.Unlock()
		return context.Canceled
	}
	if err := owner.native.Configure(command); err != nil {
		owner.mu.Unlock()
		return err
	}
	started := make(chan struct{})
	owner.pending = started
	owner.mu.Unlock()
	// A blocked Start must not hold the stop path's mutex. Termination joins
	// this attempt and checks again before accepting an empty scope.
	err := start()
	var launch lifetime.LaunchRef
	if err == nil {
		launch, err = owner.native.ObserveStarted(command.Process.Pid)
	}
	owner.mu.Lock()
	if err == nil {
		owner.launches = append(owner.launches, launch)
	}
	close(started)
	owner.pending = nil
	owner.mu.Unlock()
	return err
}

func (owner *nativeCommandLifetime) PrepareBudget(time.Time) (BudgetRequest, error) {
	return func(ctx context.Context) error { return owner.Terminate(DetachBudgetRequestContext(ctx)) }, nil
}

func (owner *nativeCommandLifetime) snapshot() ([]lifetime.LaunchRef, <-chan struct{}) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.stopped = true
	return slices.Clone(owner.launches), owner.pending
}

func (owner *nativeCommandLifetime) Terminate(ctx context.Context) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	launches, pending := owner.snapshot()
	firstErr := owner.native.Terminate(cleanup, launches)
	if pending == nil {
		return firstErr
	}
	select {
	case <-pending:
		launches, _ = owner.snapshot()
		return errors.Join(firstErr, owner.native.Terminate(cleanup, launches))
	case <-cleanup.Done():
		return errors.Join(firstErr, cleanup.Err())
	}
}

func (owner *nativeCommandLifetime) FinishCommand(ctx context.Context, _ *exec.Cmd) error {
	return owner.Terminate(ctx)
}

func (owner *nativeCommandLifetime) CloseTask(ctx context.Context) error { return owner.Terminate(ctx) }

func (owner *nativeCommandLifetime) close(ctx context.Context) error {
	if err := owner.Terminate(ctx); err != nil {
		return err
	}
	return errors.Join(owner.native.DestroyEmpty(), owner.native.Release())
}
