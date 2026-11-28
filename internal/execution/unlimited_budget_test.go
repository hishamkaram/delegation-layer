package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config/policy"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestUnlimitedBudgetDoesNotArmExpiryOrExpireAfterFormerDefault(t *testing.T) {
	clock := &countedBudgetClock{advancingClock: newAdvancingClock()}
	opts := Options{Clock: clock}
	budget := armBudgetUntil(clock, time.Time{}, opts)
	t.Cleanup(func() { budget.complete(opts); require(t, budget.await()) })
	clock.advance(24 * time.Hour)
	if clock.timers != 0 {
		t.Fatal("unlimited execution armed an expiry timer")
	}
	if _, bounded := budget.workContext.Deadline(); bounded {
		t.Fatal("unlimited work advertises a deadline")
	}
	require(t, budget.authorize(opts, "after-former-default"))
}

func TestUnlimitedBudgetRetainsExplicitOutputSilenceStop(t *testing.T) {
	clock := newSilenceTestClock()
	stopped := make(chan string, 1)
	opts := Options{Clock: clock, Stopper: silenceTestStopper{prepare: func(_ time.Time, cause string) (BudgetRequest, error) {
		return func(context.Context) error { stopped <- cause; return nil }, nil
	}}}
	budget := armBudgetUntil(clock, time.Time{}, opts)
	t.Cleanup(func() { budget.complete(opts); require(t, budget.await()) })
	budget.configureOutputSilence(time.Minute)
	budget.providerStarted()
	awaitSilenceTimer(t, clock)
	clock.advance(time.Minute)
	select {
	case cause := <-stopped:
		if cause != task.StopCauseOutputSilence {
			t.Fatalf("unlimited execution selected %q instead of output silence", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("explicit output silence did not stop unlimited work")
	}
}

func TestUnlimitedBudgetPreservesEarlierParentDeadline(t *testing.T) {
	clock := &countedBudgetClock{advancingClock: newAdvancingClock()}
	deadline := clock.Now().Add(time.Hour)
	parent, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	opts := Options{Clock: clock}
	budget := armBudgetUntilContext(parent, clock, time.Time{}, opts)
	t.Cleanup(func() { budget.complete(opts); require(t, budget.await()) })
	actual, bounded := budget.workContext.Deadline()
	if !bounded || !actual.Equal(deadline) || clock.timers != 1 {
		t.Fatal("unlimited child discarded its parent's finite bound")
	}
}

func TestUnlimitedExecutionRejectsForeignWallDeadline(t *testing.T) {
	now := time.Now()
	deadline, err := executionDeadline(policy.UnlimitedExecutionBudget(), now, now.Add(time.Minute))
	if !errors.Is(err, task.ErrIdentityMismatch) || !deadline.IsZero() {
		t.Fatalf("foreign bound changed unlimited execution: %v %v", deadline, err)
	}
	deadline, err = executionDeadline(policy.UnlimitedExecutionBudget(), now, time.Time{})
	if err != nil || !deadline.IsZero() {
		t.Fatalf("unlimited acquired a bound: %v %v", deadline, err)
	}
}
