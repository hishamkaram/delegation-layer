package resourceadmission

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// initialDeadlineBudget only needs to outlast the in-memory steps before the
// held one. The held step waits on the context instead of sleeping, so once
// it is reached the budget always ends inside it.
const initialDeadlineBudget = 100 * time.Millisecond

// blockingObserver wraps fakeObserver so a precheck or tick read can hold
// until the check's own budget ends.
type blockingObserver struct {
	*fakeObserver
	holdMemory  bool
	holdObserve bool
	onObserve   func()
}

func (b blockingObserver) hostMemory(ctx context.Context) reading[hostMemory] {
	if b.holdMemory {
		<-ctx.Done()
	}
	return b.fakeObserver.hostMemory(ctx)
}

func (b blockingObserver) observe(ctx context.Context, target *Target) snapshot {
	if b.onObserve != nil {
		b.onObserve()
	}
	if b.holdObserve {
		<-ctx.Done()
	}
	return b.fakeObserver.observe(ctx, target)
}

// heldResolver resolves testTarget, optionally after the budget ends.
type heldResolver struct {
	hold  bool
	calls int
}

func (c *heldResolver) ResolveTarget(ctx context.Context) (Target, error) {
	c.calls++
	if c.hold {
		<-ctx.Done()
	}
	return testTarget, nil
}

func deadlineFor(metrics ...Metric) map[Metric]Reason {
	out := map[Metric]Reason{}
	for _, m := range metrics {
		out[m] = ReasonDeadline
	}
	return out
}

var (
	everyMetricDeadline = deadlineFor(MetricHostCPU, MetricHostMemory, MetricCgroupCPU, MetricCgroupMemory)
	hostMetricsDeadline = deadlineFor(MetricHostCPU, MetricHostMemory)
)

func assertInitialDeadline(t *testing.T, got Assessment, err error, want map[Metric]Reason) {
	t.Helper()
	if err != nil {
		t.Fatalf("own budget expiry must not be an error: %v", err)
	}
	if got.Status() != StatusUnknown || len(got.Pressures()) != 0 {
		t.Fatalf("status = %s pressures = %v, want unknown without pressure", got.Status(), got.Pressures())
	}
	if !reflect.DeepEqual(unavailableOf(got), want) {
		t.Fatalf("unavailable = %v, want %v", got.Unavailable(), want)
	}
}

func TestOwnBudgetEndingInThePrecheckSkipsEveryLaterRead(t *testing.T) {
	cases := map[string]struct {
		untargeted bool
		want       map[Metric]Reason
	}{
		"targeted platform":   {want: everyMetricDeadline},
		"untargeted platform": {untargeted: true, want: hostMetricsDeadline},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeObserver{ticks: healthySnapshots(4), path: observed(""), untargeted: tc.untargeted}
			clk := newFakeClock()
			checker := newChecker(blockingObserver{fakeObserver: fake, holdMemory: true}, clk, initialDeadlineBudget)
			resolver := &heldResolver{}
			got, err := checker.Check(context.Background(), resolver)
			assertInitialDeadline(t, got, err, tc.want)
			if resolver.calls != 0 || len(fake.targets) != 0 || fake.membershipCalls != 0 || len(clk.sleeps) != 0 {
				t.Fatalf("resolver %d observe %d membership %d sleeps %d, want no read after the expired precheck",
					resolver.calls, len(fake.targets), fake.membershipCalls, len(clk.sleeps))
			}
		})
	}
}

func TestOwnBudgetEndingInTargetResolutionSkipsTheFirstTick(t *testing.T) {
	fake := &fakeObserver{ticks: healthySnapshots(4), path: observed("")}
	clk := newFakeClock()
	checker := newChecker(fake, clk, initialDeadlineBudget)
	resolver := &heldResolver{hold: true}
	got, err := checker.Check(context.Background(), resolver)
	assertInitialDeadline(t, got, err, everyMetricDeadline)
	if resolver.calls != 1 || len(fake.targets) != 0 || len(clk.sleeps) != 0 {
		t.Fatalf("resolver %d observe %d sleeps %d, want one resolution and no tick",
			resolver.calls, len(fake.targets), len(clk.sleeps))
	}
	if got.Scope() != (Scope{}) {
		t.Fatalf("scope = %+v, want the zero scope for a target resolved after the budget", got.Scope())
	}
}

func TestOwnBudgetEndingInTheFirstTickIsNotClear(t *testing.T) {
	cases := map[string]struct {
		untargeted bool
		want       map[Metric]Reason
	}{
		"targeted platform":   {want: everyMetricDeadline},
		"untargeted platform": {untargeted: true, want: hostMetricsDeadline},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeObserver{ticks: healthySnapshots(4), path: observed(""), untargeted: tc.untargeted}
			clk := newFakeClock()
			checker := newChecker(blockingObserver{fakeObserver: fake, holdObserve: true}, clk, initialDeadlineBudget)
			got, err := checker.Check(context.Background(), steadyResolver())
			assertInitialDeadline(t, got, err, tc.want)
			if len(fake.targets) != 1 || len(clk.sleeps) != 0 {
				t.Fatalf("observe %d sleeps %d, want the one expired tick and no wait", len(fake.targets), len(clk.sleeps))
			}
		})
	}
}

func TestCallerCancellationInTheFirstTickReturnsTheCallersError(t *testing.T) {
	fake := &fakeObserver{ticks: healthySnapshots(4), path: observed("")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checker := newChecker(blockingObserver{fakeObserver: fake, onObserve: cancel}, newFakeClock(), checkBudget)
	got, err := checker.Check(ctx, steadyResolver())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got.Status() != StatusUnknown || len(got.Unavailable()) != 0 {
		t.Fatalf("assessment = %+v, want the zero assessment with the error", got)
	}
}
