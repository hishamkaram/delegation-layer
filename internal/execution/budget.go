package execution

import (
	"context"
	"errors"
	"sync"
	"time"
)

// budgetOwner linearizes observed completion versus observed expiry. It owns
// exactly one observer, with a completion channel. Cancellation only ends the
// supervisor observation; the supervisor retains any already-started process.
type budgetOwner struct {
	mu        sync.Mutex
	completed bool
	timer     Timer
	cancel    context.CancelFunc
	done      chan struct{}
	stopErr   error
}

func armBudget(clock Clock, budget time.Duration, opts Options) (*budgetOwner, time.Time) {
	started := clock.Now()
	ctx, cancel := context.WithCancel(context.Background())
	b := &budgetOwner{timer: clock.NewTimer(budget), cancel: cancel, done: make(chan struct{})}
	ready := make(chan struct{})
	go b.observe(ctx, started.Add(budget), opts, ready)
	<-ready
	return b, started
}

func (b *budgetOwner) observe(ctx context.Context, deadline time.Time, opts Options, ready chan<- struct{}) {
	defer close(b.done)
	opts.emit("timer-armed")
	close(ready)
	select {
	case <-ctx.Done():
		return
	case <-b.timer.C():
		request, err := b.prepareExpiry(deadline, opts)
		b.stopErr = err
		if request != nil {
			b.stopErr = errors.Join(b.stopErr, request(ctx))
		}
	}
}

func (b *budgetOwner) prepareExpiry(deadline time.Time, opts Options) (BudgetRequest, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.completed {
		return nil, nil
	}
	opts.emit("deadline-observed")
	if opts.Stopper == nil {
		return nil, errors.New("budget expired without a supervisor stop capability")
	}
	// Completion may cancel observation only after durable intent is prepared.
	// No external reconciliation or stop reply is awaited under this mutex.
	return opts.Stopper.PrepareBudget(deadline)
}

func (b *budgetOwner) complete(opts Options) {
	b.mu.Lock()
	if !b.completed {
		b.completed = true
		b.timer.Stop()
		opts.emit("completion-observed")
		b.cancel()
		opts.emit("timer-disarmed")
	}
	b.mu.Unlock()
}

func (b *budgetOwner) await() error {
	<-b.done
	return b.stopErr
}
