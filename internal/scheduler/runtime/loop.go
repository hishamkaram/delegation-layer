package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

// loop is the sole writer and reader of local admission/observation state.
// Workers only return immutable results. A live ID excludes reconciliation
// from before ClaimAndArm until the attempt retires or Dispatch fully settles.
type loop struct {
	config             Config
	startup            time.Time
	live               map[string]bool
	held               map[string]time.Time
	admissions         int
	reconciling        string
	selectionAfter     string
	idleAfter          string
	observedRound      map[string]uint64
	observedDue        map[string]time.Time
	observationRound   uint64
	observationAt      time.Time
	workers            sync.WaitGroup
	results            chan taskResult
	reliabilityRunning bool
	reliabilityAfter   string
	notificationAfter  string
	reliabilityAt      time.Time
	reliabilityResults chan reliabilityResult
}

type taskResult struct {
	occurrence contract.Occurrence
	state      TaskState
	err        error
	dispatch   bool
}

// Run drives existing schedule and task owners. The caller must hold the
// scheduler owner lease throughout Run and owns closing that lease and Store.
// Cancellation stops new invocations and joins existing invocations and retained
// control work without interrupting them, then returns nil. Independently
// admitted tasks are never canceled by this loop.
func Run(ctx context.Context, config Config) (runErr error) {
	if config.Store == nil || config.Tasks == nil {
		return errors.New("scheduler runtime requires Store and Tasks")
	}
	if config.Clock == nil {
		config.Clock = nativeClock{}
	}
	if config.Waker == nil {
		config.Waker = timerWaker{}
	}
	if config.NewTaskID == nil {
		config.NewTaskID = randomid.NewRandomID
	}
	if config.NewNotificationID == nil {
		config.NewNotificationID = randomid.NewRandomID
	}
	ctx, cancel := context.WithCancel(ctx)
	l := &loop{
		config: config, startup: config.Clock.Now().UTC(), live: make(map[string]bool),
		held: make(map[string]time.Time), observedRound: make(map[string]uint64),
		observedDue: make(map[string]time.Time), results: make(chan taskResult, AdmissionLimit+1), reliabilityResults: make(chan reliabilityResult, 1),
	}
	defer func() {
		cancel()
		l.workers.Wait()
		runErr = errors.Join(runErr, l.settledErrors())
		if l.reliabilityRunning {
			runErr = errors.Join(runErr, l.finishReliability(<-l.reliabilityResults))
		}
	}()
	if err := contract.ValidateInstant("scheduler startup", l.startup); err != nil {
		return err
	}
	l.event(contract.Occurrence{}, "runtime", "ready")
	err := l.run(ctx)
	if ctx.Err() != nil && cancellationOnly(err) {
		return nil
	}
	return err
}

// Shutdown collects settled worker errors without starting recovery mutations.
// Root identity and cleanup faults remain fatal when cancellation wins the select.
func (l *loop) settledErrors() error {
	var faults error
	for {
		select {
		case result := <-l.results:
			faults = errors.Join(faults, l.reportTaskError(result))
		default:
			return faults
		}
	}
}

func (l *loop) run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := l.drain(ctx); err != nil {
			return err
		}
		l.startReliability(ctx)
		if err := l.scan(ctx); err != nil {
			return err
		}
		if err := l.wait(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (l *loop) drain(ctx context.Context) error {
	for {
		select {
		case result := <-l.reliabilityResults:
			if err := l.finishReliability(result); err != nil {
				return err
			}
		case result := <-l.results:
			if err := l.finish(ctx, result); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// wait owns and joins its interruptible wake wait even when a worker wakes
// the loop early. No waiting goroutine survives this call.
func (l *loop) wait(ctx context.Context) error {
	waitCtx, cancel := context.WithCancel(ctx)
	waited := make(chan error, 1)
	go func() { waited <- l.config.Waker.Wait(waitCtx) }()
	select {
	case err := <-waited:
		cancel()
		return err
	case result := <-l.reliabilityResults:
		cancel()
		waitErr := <-waited
		if !cancellationOnly(waitErr) {
			return waitErr
		}
		return l.finishReliability(result)
	case result := <-l.results:
		cancel()
		waitErr := <-waited
		if !cancellationOnly(waitErr) {
			return errors.Join(waitErr, l.reportTaskError(result))
		}
		return l.finish(ctx, result)
	case <-ctx.Done():
		cancel()
		return <-waited
	}
}

func (l *loop) event(occurrence contract.Occurrence, phase, code string) {
	if l.config.Report != nil {
		l.config.Report(Event{ScheduleID: occurrence.ScheduleID, TaskID: occurrence.TaskID, Phase: phase, Code: code})
	}
}

func cancellationOnly(err error) bool {
	return onlyCauses(err, context.Canceled, context.DeadlineExceeded)
}

// Domain conflicts and cancellation can be wrapped or joined. A joined disk
// or invariant fault must never be mistaken for an expected race or shutdown.
func onlyCauses(err error, allowed ...error) bool {
	if err == nil {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if !onlyCauses(cause, allowed...) {
				return false
			}
		}
		return len(joined.Unwrap()) > 0
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return onlyCauses(wrapped, allowed...)
	}
	for _, cause := range allowed {
		if errors.Is(err, cause) {
			return true
		}
	}
	return false
}

func storeError(operation string, occurrence contract.Occurrence, err error) error {
	return fmt.Errorf("scheduler %s schedule %s task %s: %w", operation, occurrence.ScheduleID, occurrence.TaskID, err)
}
