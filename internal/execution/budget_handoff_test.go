package execution

import (
	"context"
	"errors"
	"sync"
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
	var persistOnce sync.Once
	release := func() { persistOnce.Do(func() { close(persist) }) }
	t.Cleanup(func() {
		release()
		owner.complete(opts)
		select {
		case <-owner.done:
		case <-time.After(5 * time.Second):
			t.Error("budget observer did not join")
		}
	})
	clock.timer.ch <- clock.now.Add(time.Second)
	select {
	case <-preparing:
	case <-owner.done:
		t.Fatalf("budget observer returned before preparation: %v", owner.await())
	case <-time.After(5 * time.Second):
		t.Fatal("budget observer did not reach preparation")
	}
	t.Cleanup(func() {
		release()
		select {
		case <-completed:
		case <-time.After(5 * time.Second):
			t.Error("budget completion did not join")
		}
	})
	go func() {
		close(completionAttempt)
		owner.complete(opts)
		close(completed)
	}()
	<-completionAttempt
	release()
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
	operated := make(chan struct{}, 1)
	opts := Options{Stopper: budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
		close(prepared)
		return func(context.Context) error { operated <- struct{}{}; return nil }, failed
	})}
	owner, _ := armBudget(clock, time.Second, opts)
	t.Cleanup(func() { owner.complete(opts); <-owner.done })
	clock.timer.ch <- clock.now.Add(time.Second)
	// A failed preparation can finish the observer before the test resumes.
	// Join first, then check both facts instead of racing their ready channels.
	select {
	case <-owner.done:
	case <-time.After(5 * time.Second):
		t.Fatal("budget observer did not finish failed preparation")
	}
	select {
	case <-prepared:
	default:
		t.Fatal("budget observer returned without preparation")
	}
	owner.complete(opts)
	if !errors.Is(owner.await(), failed) {
		t.Fatal("lost durable preparation failure")
	}
	select {
	case <-operated:
		t.Fatal("failed preparation granted a supervisor operation")
	default:
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

	ownerContext := context.WithValue(ctx, budgetRequestContextMarker, true)
	detached := DetachBudgetRequestContext(ownerContext)
	cancel()
	if detached.Err() != nil || detached.Done() != nil {
		t.Fatalf("owner context remained cancelable: err=%v done=%v", detached.Err(), detached.Done())
	}
}
