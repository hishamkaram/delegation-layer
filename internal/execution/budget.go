package execution

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
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
	expired         bool
	prepared        bool
	timer           Timer
	cancel          context.CancelFunc
	workContext     context.Context
	workCancel      context.CancelFunc
	wake            chan struct{}
	preparedDone    chan struct{}
	done            chan struct{}
	stopErr         error
	cause           string
	silenceDuration time.Duration
	silenceActive   bool
	lastOutput      time.Time
	activity        chan struct{}
}

func armBudget(clock Clock, budget time.Duration, opts Options) (*budgetOwner, time.Time) {
	started := clock.Now()
	return armBudgetUntil(clock, started.Add(budget), opts), started
}

func armBudgetUntil(clock Clock, deadline time.Time, opts Options) *budgetOwner {
	return armBudgetUntilContext(context.Background(), clock, deadline, opts)
}

func armBudgetUntilContext(parent context.Context, clock Clock, deadline time.Time, opts Options) *budgetOwner {
	// Stop observation is independently joined by the budget owner. Stop
	// requests receive caller cancellation; native owners detach bounded cleanup.
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stopContext, stopCancel := context.WithCancel(parent)
	if inherited, ok := parent.Deadline(); ok && inherited.Before(deadline) {
		deadline = inherited
	}
	workContext, workCancel := context.WithCancel(deadlineContext{Context: parent, deadline: deadline})
	b := &budgetOwner{
		clock:        clock,
		deadline:     deadline,
		timer:        clock.NewTimer(max(0, deadline.Sub(clock.Now()))),
		cancel:       func() { cancel(); stopCancel() },
		workContext:  workContext,
		workCancel:   workCancel,
		wake:         make(chan struct{}, 1),
		activity:     make(chan struct{}, 1),
		cause:        "budget",
		preparedDone: make(chan struct{}),
		done:         make(chan struct{}),
	}
	ready := make(chan struct{})
	go b.observe(ctx, stopContext, b.deadline, opts, ready)
	<-ready
	return b
}

func (b *budgetOwner) observe(observerContext, ctx context.Context, deadline time.Time, opts Options, ready chan<- struct{}) {
	defer close(b.done)
	opts.emit("timer-armed")
	close(ready)
	if !b.awaitExpiry(observerContext, opts) {
		return
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
	var request BudgetRequest
	var err error
	if b.cause == task.StopCauseOutputSilence {
		if stopper, ok := opts.Stopper.(CauseStopper); ok {
			request, err = stopper.PrepareStop(b.lastOutput.Add(b.silenceDuration), b.cause)
		} else {
			err = errors.New("output silence expired without a cause-aware stop capability")
		}
	} else {
		request, err = opts.Stopper.PrepareBudget(deadline)
	}
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
// observation and a start authorization. Once authorization wins, the caller
// owns the one permitted start even if expiry is observed while that start is
// in progress; the supervisor stop path remains responsible for the process.
func (b *budgetOwner) authorizeStart(opts Options) error {
	return b.authorize(opts, "start-authorized")
}

// authorizeAndStart reserves the one native start while holding the budget
// mutex, then invokes the callback without the mutex. This lets the timer
// observer prepare supervisor stopping while a platform start call is blocked.
// If authorization wins first, a later expiry does not grant a second start or
// cancel the already-authorized callback; it only routes through supervision.
func (b *budgetOwner) authorizeAndStart(opts Options, start func() error) error {
	if start == nil {
		return errBudgetExpiredBeforeStart
	}
	b.mu.Lock()
	if b.completed || b.expired || b.prepared || b.observeClockExpiryLocked(opts) {
		b.mu.Unlock()
		return b.authorize(opts, "start-authorized")
	}
	if err := b.workContext.Err(); err != nil {
		b.mu.Unlock()
		return err
	}
	opts.emit("start-authorized")
	b.mu.Unlock()
	startErr := start()
	return startErr
}

func (b *budgetOwner) authorize(opts Options, event string) error {
	b.mu.Lock()
	if b.completed {
		b.mu.Unlock()
		return errBudgetExpiredBeforeStart
	}
	clockExpired := b.observeClockExpiryLocked(opts)
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
	if err := b.workContext.Err(); err != nil {
		b.mu.Unlock()
		return err
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
		if !b.expired && b.observeClockExpiryLocked(opts) {
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

// awaitExpiry is the sole timer observer for wall and output-silence expiry.
// Activity changes only its monotonic deadline; it cannot revoke a selected stop.
func (b *budgetOwner) awaitExpiry(ctx context.Context, opts Options) bool {
	var silence Timer
	var silenceC <-chan time.Time
	defer func() {
		if silence != nil {
			silence.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-b.timer.C():
			return true
		case <-b.wake:
			return true
		case <-b.activity:
		case <-silenceC:
		}
		b.mu.Lock()
		if silence != nil {
			silence.Stop()
			silence = nil
			silenceC = nil
		}
		now := b.clock.Now()
		if !b.completed && !b.expired && b.silenceActive && b.silenceDuration > 0 {
			deadline := b.lastOutput.Add(b.silenceDuration)
			if !now.Before(deadline) {
				b.expired = true
				if now.Before(b.deadline) {
					b.cause = task.StopCauseOutputSilence
				}
				b.workCancel()
				opts.emit("output-silence-observed")
				b.mu.Unlock()
				return true
			}
			silence = b.clock.NewTimer(deadline.Sub(now))
			silenceC = silence.C()
		}
		b.mu.Unlock()
	}
}

func (b *budgetOwner) configureOutputSilence(duration time.Duration) {
	b.mu.Lock()
	b.silenceDuration = duration
	b.mu.Unlock()
}

func (b *budgetOwner) providerStarted() {
	b.mu.Lock()
	if b.silenceDuration > 0 && !b.completed && !b.expired {
		b.silenceActive = true
		if b.lastOutput.IsZero() {
			b.lastOutput = b.clock.Now()
		}
	}
	b.mu.Unlock()
	b.wakeActivity()
}

func (b *budgetOwner) outputActivity() {
	b.mu.Lock()
	if b.silenceDuration > 0 && !b.completed && !b.expired {
		now := b.clock.Now()
		// Delayed timer delivery cannot let overdue output renew an elapsed window.
		if !b.silenceActive || now.Before(b.lastOutput.Add(b.silenceDuration)) {
			b.lastOutput = now
		}
	}
	b.mu.Unlock()
	b.wakeActivity()
}

func (b *budgetOwner) providerWaited() {
	b.mu.Lock()
	b.silenceActive = false
	b.mu.Unlock()
	b.wakeActivity()
}

func (b *budgetOwner) wakeActivity() {
	select {
	case b.activity <- struct{}{}:
	default:
	}
}

// Authorization and completion cannot let delayed timer delivery extend an
// elapsed deadline. The caller holds mu; wall expiry wins a simultaneous tie.
func (b *budgetOwner) observeClockExpiryLocked(opts Options) bool {
	now := b.clock.Now()
	wall := !now.Before(b.deadline)
	silence := b.silenceActive && b.silenceDuration > 0 && !now.Before(b.lastOutput.Add(b.silenceDuration))
	if !wall && !silence {
		return false
	}
	if !b.expired {
		b.expired = true
		if !wall {
			b.cause = task.StopCauseOutputSilence
		}
		b.workCancel()
		opts.emit("deadline-observed")
	}
	return true
}
