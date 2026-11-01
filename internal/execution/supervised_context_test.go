package execution

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

type supervisedContextKey struct{}

type supervisedContextLifetime struct {
	close func(context.Context) error
}

func (supervisedContextLifetime) StartCommand(_ *exec.Cmd, start func() error) error {
	return start()
}

func (supervisedContextLifetime) FinishCommand(context.Context, *exec.Cmd) error { return nil }
func (supervisedContextLifetime) Terminate(context.Context) error                { return nil }
func (owner supervisedContextLifetime) CloseTask(ctx context.Context) error      { return owner.close(ctx) }

func supervisedContextNoExpiry(t *testing.T) Stopper {
	t.Helper()
	return budgetPreparerFunc(func(time.Time) (BudgetRequest, error) {
		t.Error("cancellation or early completion invented budget expiry")
		return nil, errors.New("unexpected budget expiry")
	})
}

func TestRunSupervisedContextRejectsInvalidWorkContexts(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		want error
	}{
		{name: "missing context"},
		{name: "already canceled", ctx: canceled, want: context.Canceled},
	}
	for _, input := range cases {
		t.Run(input.name, func(t *testing.T) {
			workErr, stopErr := RunSupervisedContext(input.ctx, time.Now().Add(time.Minute), SupervisedOptions{Stopper: supervisedContextNoExpiry(t)}, func(PreflightScope) error {
				t.Error("invalid caller context entered work")
				return nil
			})
			if workErr == nil || stopErr != nil {
				t.Fatalf("invalid context result: work=%v stop=%v", workErr, stopErr)
			}
			if input.want != nil && !errors.Is(workErr, input.want) {
				t.Fatalf("invalid context lost its error: got %v, want %v", workErr, input.want)
			}
		})
	}
}

func TestRunSupervisedContextInheritsValuesAndRevokesStart(t *testing.T) {
	parent := context.WithValue(t.Context(), supervisedContextKey{}, "caller-value")
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	clock := &countedBudgetClock{advancingClock: newAdvancingClock()}
	closed, starts := 0, 0
	lifetime := supervisedContextLifetime{close: func(cleanup context.Context) error {
		closed++
		if cleanup.Err() != nil || cleanup.Done() != nil || cleanup.Value(supervisedContextKey{}) != "caller-value" {
			t.Errorf("cleanup lost its detached owner scope: %v", cleanup.Err())
		}
		return nil
	}}
	workErr, stopErr := RunSupervisedContext(ctx, clock.Now().Add(time.Minute), SupervisedOptions{
		Clock: clock, Stopper: supervisedContextNoExpiry(t), Lifetime: lifetime,
	}, func(scope PreflightScope) error {
		if scope.Context().Value(supervisedContextKey{}) != "caller-value" {
			t.Error("work context lost caller values")
		}
		cancel()
		if !errors.Is(scope.Context().Err(), context.Canceled) {
			t.Error("work context did not inherit cancellation")
		}
		return scope.Start(func() error { starts++; return nil })
	})
	if !errors.Is(workErr, context.Canceled) || stopErr != nil || starts != 0 || closed != 1 || clock.timers != 1 {
		t.Fatalf("cancellation failed to revoke start and join cleanup: work=%v stop=%v starts=%d closes=%d timers=%d", workErr, stopErr, starts, closed, clock.timers)
	}
}

func TestRunSupervisedContextEarlierParentDeadlineReachesStopOwnerDuringWork(t *testing.T) {
	parent, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	deadline, ok := parent.Deadline()
	if !ok {
		t.Fatal("parent deadline is missing")
	}
	stopped := make(chan time.Time, 1)
	watchdog := time.NewTimer(3 * time.Second)
	defer watchdog.Stop()
	stopper := budgetPreparerFunc(func(observed time.Time) (BudgetRequest, error) {
		stopped <- observed
		return func(ctx context.Context) error {
			if actual, hasDeadline := ctx.Deadline(); !hasDeadline || !actual.Equal(deadline) {
				t.Error("normal stop observation lost inherited deadline")
			}
			return nil
		}, nil
	})
	workErr, stopErr := RunSupervisedContext(parent, deadline.Add(time.Hour), SupervisedOptions{Stopper: stopper}, func(scope PreflightScope) error {
		if actual, hasDeadline := scope.Context().Deadline(); !hasDeadline || !actual.Equal(deadline) {
			t.Error("work advertised the later deadline")
		}
		// Work remains blocked until the real clock invokes the stop owner. Its
		// own context cancellation cannot make this test accidentally pass.
		select {
		case observed := <-stopped:
			if !observed.Equal(deadline) {
				t.Errorf("stop prepared for %s, want parent deadline %s", observed, deadline)
			}
			return nil
		case <-watchdog.C:
			return errors.New("inherited deadline did not reach the stop owner during work")
		}
	})
	if !errors.Is(workErr, errBudgetExpiredBeforeStart) || stopErr != nil {
		t.Fatalf("earlier parent deadline did not own actual expiry: work=%v stop=%v", workErr, stopErr)
	}
}

func TestRunSupervisedContextJoinsCanceledWorkAndCleanupFault(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cleanupFault := errors.New("owned cleanup failure")
	lifetime := supervisedContextLifetime{close: func(cleanup context.Context) error {
		if cleanup.Err() != nil {
			t.Error("caller cancellation revoked owned cleanup")
		}
		return cleanupFault
	}}
	workErr, stopErr := RunSupervisedContext(ctx, time.Now().Add(time.Minute), SupervisedOptions{
		Stopper: supervisedContextNoExpiry(t), Lifetime: lifetime,
	}, func(PreflightScope) error { cancel(); return nil })
	if !errors.Is(workErr, context.Canceled) || !errors.Is(workErr, cleanupFault) || stopErr != nil {
		t.Fatalf("lost cancellation/cleanup cause: work=%v stop=%v", workErr, stopErr)
	}
}
