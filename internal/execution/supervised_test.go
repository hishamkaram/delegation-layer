package execution

import (
	"context"
	"errors"
	"testing"
	"time"
)

type countedBudgetClock struct {
	*advancingClock
	timers int
}

func (c *countedBudgetClock) NewTimer(d time.Duration) Timer {
	c.timers++
	return c.advancingClock.NewTimer(d)
}

func TestSupervisedScopeBorrowsOneDeadlineAndExpiresOnReturn(t *testing.T) {
	clock := &countedBudgetClock{advancingClock: newAdvancingClock()}
	deadline := clock.Now().Add(time.Minute)
	var saved PreflightScope
	workErr, stopErr := RunSupervised(deadline, SupervisedOptions{
		Clock: clock,
		Stopper: budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
			t.Error("completed inspection requested a stop")
			return nil, errors.New("unexpected stop")
		}),
	}, func(scope PreflightScope) error {
		saved = scope
		if actual, ok := scope.Context().Deadline(); !ok || !actual.Equal(deadline) {
			t.Error("scope lost the persisted absolute deadline")
		}
		return scope.Authorize()
	})
	if workErr != nil || stopErr != nil || clock.timers != 1 {
		t.Fatalf("work=%v stop=%v timers=%d", workErr, stopErr, clock.timers)
	}
	if saved.Context().Err() == nil || saved.Authorize() == nil {
		t.Fatal("retained callback scope still authorizes work")
	}
}

func TestSupervisedQueuedExpiryDoesNotEnterWork(t *testing.T) {
	clock := newAdvancingClock()
	preparations := 0
	workErr, stopErr := RunSupervised(clock.Now(), SupervisedOptions{
		Clock: clock,
		Stopper: budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
			preparations++
			return func(context.Context) error { return nil }, nil
		}),
	}, func(PreflightScope) error {
		t.Error("expired queued worker entered native work")
		return nil
	})
	if !errors.Is(workErr, errBudgetExpiredBeforeStart) || stopErr != nil || preparations != 1 {
		t.Fatalf("work=%v stop=%v preparations=%d", workErr, stopErr, preparations)
	}
}

func TestSupervisedLateSuccessCannotBecomeEligible(t *testing.T) {
	clock := newAdvancingClock()
	preparations := 0
	workErr, stopErr := RunSupervised(clock.Now().Add(time.Minute), SupervisedOptions{
		Clock: clock,
		Stopper: budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
			preparations++
			return func(context.Context) error { return nil }, nil
		}),
	}, func(PreflightScope) error {
		clock.advance(time.Minute)
		return nil
	})
	if !errors.Is(workErr, errBudgetExpiredBeforeStart) || stopErr != nil || preparations != 1 {
		t.Fatalf("late success accepted: work=%v stop=%v preparations=%d", workErr, stopErr, preparations)
	}
}

type steppingCompletionClock struct {
	*advancingClock
	stepAfterRead bool
}

func (c *steppingCompletionClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now
	if c.stepAfterRead {
		c.now = c.now.Add(time.Minute)
		c.stepAfterRead = false
	}
	return now
}

func TestSupervisedExpiryBetweenFinalGateAndCompletionIsRejected(t *testing.T) {
	clock := &steppingCompletionClock{advancingClock: newAdvancingClock()}
	workErr, stopErr := RunSupervised(clock.Now().Add(time.Minute), SupervisedOptions{
		Clock: clock,
		Stopper: budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
			return func(context.Context) error { return nil }, nil
		}),
	}, func(PreflightScope) error {
		clock.mu.Lock()
		clock.stepAfterRead = true
		clock.mu.Unlock()
		return nil
	})
	if !errors.Is(workErr, errBudgetExpiredBeforeStart) || stopErr != nil {
		t.Fatalf("completion lost expiry: work=%v stop=%v", workErr, stopErr)
	}
}

func TestSupervisedExpiryCancelsObservationButWaitsForOwnedWork(t *testing.T) {
	clock := newAdvancingClock()
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		workErr, stopErr := RunSupervised(clock.Now().Add(time.Minute), SupervisedOptions{
			Clock: clock,
			Stopper: budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
				return func(context.Context) error { return nil }, nil
			}),
		}, func(scope PreflightScope) error {
			close(entered)
			<-scope.Context().Done()
			close(canceled)
			<-release
			return nil
		})
		finished <- errors.Join(workErr, stopErr)
	}()
	<-entered
	clock.advance(time.Minute)
	clock.timer.ch <- clock.Now()
	<-canceled
	select {
	case <-finished:
		t.Fatal("expiry abandoned owned inspection work")
	default:
	}
	close(release)
	if err := <-finished; !errors.Is(err, errBudgetExpiredBeforeStart) {
		t.Fatalf("expired work became eligible: %v", err)
	}
}
