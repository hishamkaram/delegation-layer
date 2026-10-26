package resourceadmission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func criticalMemory() reading[hostMemory] { return availableMemory(gib/2 - 1) }

func TestCriticalPrecheckDecidesBeforeResolverCPUMembershipOrCgroupReads(t *testing.T) {
	ticks := healthySnapshots(4)
	ticks[0].memory = criticalMemory()
	checker, obs, clk := newTestChecker(t, ticks)
	resolver := steadyResolver()
	got, err := checker.Check(context.Background(), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 0 || len(obs.targets) != 0 || obs.membershipCalls != 0 || len(clk.sleeps) != 0 || obs.memoryCalls != 1 {
		t.Fatalf("resolver %d observe %d membership %d sleeps %d memory %d, want only one memory read",
			resolver.calls, len(obs.targets), obs.membershipCalls, len(clk.sleeps), obs.memoryCalls)
	}
	if pressuresOf(got)[MetricHostMemory] != BasisCritical || len(got.Pressures()) != 1 {
		t.Fatalf("pressures = %v, want exactly critical host memory", got.Pressures())
	}
	if len(got.Unavailable()) != 0 {
		t.Fatalf("unavailable = %v, want skipped metrics not reported as missing", got.Unavailable())
	}
	if got.Status() != StatusPressure {
		t.Fatalf("status = %s", got.Status())
	}
}

func TestNonCriticalPrecheckIsDiscardedAndTheBaselineFlowIsUnchanged(t *testing.T) {
	cases := map[string]reading[hostMemory]{
		"hot":     availableMemory(gib - 1),
		"unknown": unobserved[hostMemory](ReasonUnreadable),
		"healthy": availableMemory(8 * gib),
	}
	for name, precheck := range cases {
		t.Run(name, func(t *testing.T) {
			ticks := withCPU(healthySnapshots(4), cpuSeries(0.99, 0.99, 0.99))
			checker, obs, clk := newTestChecker(t, ticks)
			obs.precheck = &precheck
			got, err := checker.Check(context.Background(), steadyResolver())
			if err != nil {
				t.Fatal(err)
			}
			if pressuresOf(got)[MetricHostCPU] != BasisSustained || len(clk.sleeps) != 3 || len(obs.targets) != 4 {
				t.Fatalf("pressures = %v sleeps %d observations %d, want CPU to win after the unchanged sequence",
					got.Pressures(), len(clk.sleeps), len(obs.targets))
			}
			if len(got.Unavailable()) != 0 {
				t.Fatalf("unavailable = %v, want no stale warning from the discarded precheck", got.Unavailable())
			}
		})
	}
}

func TestFailedPrecheckThenHealthyMeasurementIsClean(t *testing.T) {
	checker, obs, _ := newTestChecker(t, healthySnapshots(4))
	failed := unobserved[hostMemory](ReasonUnreadable)
	obs.precheck = &failed
	got, err := checker.Check(context.Background(), nil)
	if err != nil || got.Status() != StatusClear || len(got.Unavailable()) != 0 {
		t.Fatalf("status = %s unavailable = %v err = %v, want clear", got.Status(), got.Unavailable(), err)
	}
}

func TestPrecheckHotIsNotACountedWarningSample(t *testing.T) {
	low, ok := availableMemory(gib-1), availableMemory(2*gib)
	ticks := healthySnapshots(4)
	for i, sample := range []reading[hostMemory]{ok, low, low, ok} {
		ticks[i].memory = sample
	}
	checker, obs, _ := newTestChecker(t, ticks)
	obs.precheck = &low
	got, err := checker.Check(context.Background(), nil)
	if err != nil || got.Status() != StatusClear {
		t.Fatalf("status = %s err = %v, want two real hot intervals to stay clear", got.Status(), err)
	}
}

func TestPrecheckCancellationReturnsTheCallersError(t *testing.T) {
	for name, memory := range map[string]reading[hostMemory]{"healthy": availableMemory(8 * gib), "critical": criticalMemory()} {
		t.Run(name, func(t *testing.T) {
			ticks := healthySnapshots(4)
			ticks[0].memory = memory
			checker, obs, _ := newTestChecker(t, ticks)
			ctx, cancel := context.WithCancel(context.Background())
			obs.onMemory = cancel
			got, err := checker.Check(ctx, steadyResolver())
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if got.Status() != StatusUnknown || len(got.Pressures()) != 0 {
				t.Fatalf("assessment = %+v, want the zero assessment with the error", got)
			}
		})
	}
}

func TestCriticalPrecheckSurvivesTheChecksOwnBudgetExpiring(t *testing.T) {
	ticks := healthySnapshots(4)
	ticks[0].memory = criticalMemory()
	obs := &fakeObserver{ticks: ticks, path: observed(""), onMemory: func() { time.Sleep(30 * time.Millisecond) }}
	checker := newChecker(obs, newFakeClock(), time.Millisecond)
	got, err := checker.Check(context.Background(), nil)
	if err != nil || pressuresOf(got)[MetricHostMemory] != BasisCritical {
		t.Fatalf("pressures = %v err = %v, want the observed critical reading to stand", got.Pressures(), err)
	}
}
