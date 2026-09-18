package execution

import (
	"context"
	"errors"
	"sync"
	"time"
)

var errBudgetExpiredBeforeStart = errors.New("execution budget expired before provider start")

// budgetOwner linearizes observed completion versus observed expiry. It owns
// exactly one observer, with a completion channel. Cancellation only ends the
// supervisor observation; the supervisor retains any already-started process.
type budgetOwner struct {
	mu        sync.Mutex
	clock     Clock
	deadline  time.Time
	completed bool
	// expired records the arbitration decision; prepared records that the
	// observer has durably prepared the single supervisor request.
	expired      bool
	prepared     bool
	timer        Timer
	cancel       context.CancelFunc
	workContext  context.Context
	workCancel   context.CancelFunc
	wake         chan struct{}
	preparedDone chan struct{}
	done         chan struct{}
	stopErr      error
}

func armBudget(clock Clock, budget time.Duration, opts Options) (*budgetOwner, time.Time) {
	started := clock.Now()
	return armBudgetUntil(clock, started.Add(budget), opts), started
}

func armBudgetUntil(clock Clock, deadline time.Time, opts Options) *budgetOwner {
	ctx, cancel := context.WithCancel(context.Background())
	workContext, workCancel := context.WithCancel(deadlineContext{Context: context.Background(), deadline: deadline})
	b := &budgetOwner{
		clock:        clock,
		deadline:     deadline,
		timer:        clock.NewTimer(max(0, deadline.Sub(clock.Now()))),
		cancel:       cancel,
		workContext:  workContext,
		workCancel:   workCancel,
		wake:         make(chan struct{}, 1),
		preparedDone: make(chan struct{}),
		done:         make(chan struct{}),
	}
	ready := make(chan struct{})
	go b.observe(ctx, b.deadline, opts, ready)
	<-ready
	return b
}

func (b *budgetOwner) observe(ctx context.Context, deadline time.Time, opts Options, ready chan<- struct{}) {
	defer close(b.done)
	opts.emit("timer-armed")
	close(ready)
	select {
	case <-ctx.Done():
		return
	case <-b.timer.C():
	case <-b.wake:
	}
	request, err := b.prepareExpiry(deadline, opts)
	b.recordStopError(err)
	if request != nil {
		requestContext := context.WithValue(ctx, budgetRequestContextMarker, true)
		b.recordStopError(request(requestContext))
	}
}

func (b *budgetOwner) prepareExpiry(deadline time.Time, opts Options) (BudgetRequest, error) {
	b.mu.Lock()
	if b.completed || b.prepared {
		b.mu.Unlock()
		return nil, nil
	}
	if !b.expired {
		b.expired = true
		opts.emit("deadline-observed")
	}
	b.workCancel()
	if opts.Stopper == nil {
		b.prepared = true
		close(b.preparedDone)
		b.mu.Unlock()
		return nil, errors.New("budget expired without a supervisor stop capability")
	}
	request, err := opts.Stopper.PrepareBudget(deadline)
	b.prepared = true
	close(b.preparedDone)
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return request, nil
}

// authorizeStart is the final arbitration point after policy preflight and
// immediately before Cmd.Start. The mutex defines the winner between a timer
// observation and a start authorization. It is released before the caller
// enters Cmd.Start, so a blocking start hook cannot delay budget preparation.
func (b *budgetOwner) authorizeStart(opts Options) error {
	return b.authorize(opts, "start-authorized")
}

func (b *budgetOwner) authorize(opts Options, event string) error {
	b.mu.Lock()
	if b.completed {
		b.mu.Unlock()
		return errBudgetExpiredBeforeStart
	}
	clockExpired := !b.clock.Now().Before(b.deadline)
	if b.expired || b.prepared || clockExpired {
		if !b.expired {
			b.expired = true
			opts.emit("deadline-observed")
		}
		b.workCancel()
		prepared := b.preparedDone
		needsWake := !b.prepared
		b.mu.Unlock()
		if needsWake {
			select {
			case b.wake <- struct{}{}:
			default:
			}
		}
		<-prepared
		return errBudgetExpiredBeforeStart
	}
	opts.emit(event)
	b.mu.Unlock()
	return nil
}

func (b *budgetOwner) recordStopError(err error) {
	if err == nil {
		return
	}
	b.mu.Lock()
	b.stopErr = errors.Join(b.stopErr, err)
	b.mu.Unlock()
}

func (b *budgetOwner) complete(opts Options) {
	for {
		b.mu.Lock()
		if b.completed {
			b.mu.Unlock()
			return
		}
		// A delayed timer delivery cannot let work completed after the
		// deadline cancel the observer before it records expiry.
		if !b.expired && !b.clock.Now().Before(b.deadline) {
			b.expired = true
			b.workCancel()
			opts.emit("deadline-observed")
			select {
			case b.wake <- struct{}{}:
			default:
			}
		}
		if b.expired && !b.prepared {
			prepared := b.preparedDone
			b.mu.Unlock()
			<-prepared
			continue
		}
		b.completed = true
		b.workCancel()
		b.timer.Stop()
		opts.emit("completion-observed")
		b.cancel()
		opts.emit("timer-disarmed")
		b.mu.Unlock()
		return
	}
}

func (b *budgetOwner) await() error {
	<-b.done
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopErr
}
