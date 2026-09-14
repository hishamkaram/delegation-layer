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
