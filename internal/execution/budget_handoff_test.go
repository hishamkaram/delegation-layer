package execution

import (
	"context"
	"errors"
	"testing"
	"time"
)

type budgetPreparerFunc func(time.Time) (BudgetRequest, error)

func (f budgetPreparerFunc) PrepareBudget(d time.Time) (BudgetRequest, error) { return f(d) }

func TestBudgetExpiryPersistsBeforeCompletionCancelsObservation(t *testing.T) {
	clock := newManualClock()
	events := &eventLog{}
	preparing := make(chan struct{})
	persist := make(chan struct{})
	completionAttempt := make(chan struct{})
	completed := make(chan struct{})
	opts := Options{Hooks: Hooks{Event: events.add}, Stopper: budgetPreparerFunc(func(deadline time.Time) (BudgetRequest, error) {
		if !deadline.Equal(clock.now.Add(time.Second)) {
			return nil, errors.New("wrong deadline")
		}
		close(preparing)
		<-persist
		events.add("intent-persisted")
		return func(ctx context.Context) error {
			<-ctx.Done()
			events.add("observation-released")
			return nil
		}, nil
	})}
	owner, _ := armBudget(clock, time.Second, opts)
	clock.timer.ch <- clock.now.Add(time.Second)
	<-preparing
	go func() {
		close(completionAttempt)
		owner.complete(opts)
		close(completed)
	}()
	<-completionAttempt
	close(persist)
	<-completed
	require(t, owner.await())
	assertOrder(t, events.snapshot(), "deadline-observed", "intent-persisted")
	assertOrder(t, events.snapshot(), "intent-persisted", "completion-observed")
	assertOrder(t, events.snapshot(), "completion-observed", "observation-released")
}

func TestBudgetPreparationFailurePreventsSupervisorOperation(t *testing.T) {
	clock := newManualClock()
	failed := errors.New("durable stop intent failed")
	prepared := make(chan struct{})
	opts := Options{Stopper: budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
		close(prepared)
		return nil, failed
	})}
	owner, _ := armBudget(clock, time.Second, opts)
	clock.timer.ch <- clock.now.Add(time.Second)
	<-prepared
	owner.complete(opts)
	if !errors.Is(owner.await(), failed) {
		t.Fatal("lost durable preparation failure")
	}
}

func TestBudgetCompletionPreventsEvenPreparation(t *testing.T) {
	clock := newManualClock()
	called := make(chan struct{}, 1)
	opts := Options{Stopper: budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
		called <- struct{}{}
		return nil, errors.New("unexpected preparation")
	})}
	owner, _ := armBudget(clock, time.Second, opts)
	owner.complete(opts)
	clock.timer.ch <- clock.now.Add(time.Second)
	require(t, owner.await())
	select {
	case <-called:
		t.Fatal("completion-first created budget intent")
	default:
	}
}

func TestClockExpiryAtCompletionCannotCancelUndeliveredTimer(t *testing.T) {
	clock := newAdvancingClock()
	events := &eventLog{}
	preparations, requests := 0, 0
	opts := Options{Hooks: Hooks{Event: events.add}, Stopper: budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
		preparations++
		events.add("intent-persisted")
		return func(context.Context) error { requests++; return nil }, nil
	})}
	owner, _ := armBudget(clock, time.Second, opts)
	require(t, owner.authorizeStart(opts))
	clock.advance(time.Second)
	owner.complete(opts)
	require(t, owner.await())
	if preparations != 1 || requests != 1 {
		t.Fatalf("late completion lost expiry: preparations=%d requests=%d", preparations, requests)
	}
	assertOrder(t, events.snapshot(), "intent-persisted", "completion-observed")
}

func TestBudgetPreparationErrorCannotGrantReturnedRequest(t *testing.T) {
	clock := newAdvancingClock()
	failure := errors.New("stop intent barrier failed")
	requests := 0
	opts := Options{Stopper: budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
		return func(context.Context) error { requests++; return nil }, failure
	})}
	owner, _ := armBudget(clock, time.Second, opts)
	clock.advance(time.Second)
	owner.complete(opts)
	if !errors.Is(owner.await(), failure) {
		t.Fatal("lost failed preparation")
	}
	if requests != 0 {
		t.Fatal("failed preparation granted a supervisor operation")
	}
}

func TestDetachBudgetRequestContextOnlyDetachesOwnerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if got := DetachBudgetRequestContext(ctx); got != ctx {
		t.Fatal("ordinary caller context was detached")
	}

	ownerContext := budgetRequestContext{Context: ctx}
	detached := DetachBudgetRequestContext(ownerContext)
	cancel()
	if detached.Err() != nil || detached.Done() != nil {
		t.Fatalf("owner context remained cancelable: err=%v done=%v", detached.Err(), detached.Done())
	}
}
