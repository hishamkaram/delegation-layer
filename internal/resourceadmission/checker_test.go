package resourceadmission

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

var oneSecond = []time.Duration{time.Second, time.Second, time.Second}

func withCPU(ticks []snapshot, cpus []reading[cpuCounters]) []snapshot {
	for i := range ticks {
		ticks[i].cpu = cpus[i]
	}
	return ticks
}

func TestCheckHealthyFirstPositiveIntervalFinishesAfterOneSecond(t *testing.T) {
	checker, obs, clk := newTestChecker(t, healthySnapshots(4))
	got, err := checker.Check(context.Background(), nil)
	if err != nil {
		t.Fatalf("Check error = %v", err)
	}
	if got.Status() != StatusClear || len(got.Unavailable()) != 0 {
		t.Fatalf("status = %s unavailable = %v, want clear", got.Status(), got.Unavailable())
	}
	if !reflect.DeepEqual(clk.sleeps, oneSecond[:1]) || len(obs.targets) != 2 {
		t.Fatalf("sleeps = %v observations = %d, want one wait and a baseline plus one interval", clk.sleeps, len(obs.targets))
	}
}

func TestCheckBaselineWarningIsNotCountedTowardTheThreeIntervals(t *testing.T) {
	low, ok := availableMemory(gib-1), availableMemory(2*gib)
	cases := []struct {
		name    string
		samples [4]reading[hostMemory]
		want    Status
		sleeps  int
	}{
		{"low baseline then healthy interval", [4]reading[hostMemory]{low, ok, ok, ok}, StatusClear, 1},
		{"low baseline plus two low intervals", [4]reading[hostMemory]{low, low, low, ok}, StatusClear, 3},
		{"low baseline plus three low intervals", [4]reading[hostMemory]{low, low, low, low}, StatusPressure, 3},
		{"healthy baseline plus three low intervals", [4]reading[hostMemory]{ok, low, low, low}, StatusPressure, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ticks := healthySnapshots(4)
			for i := range ticks {
				ticks[i].memory = tc.samples[i]
			}
			checker, _, clk := newTestChecker(t, ticks)
			got, err := checker.Check(context.Background(), nil)
			if err != nil || got.Status() != tc.want || len(clk.sleeps) != tc.sleeps {
				t.Fatalf("status = %s err = %v sleeps = %d, want %s with %d", got.Status(), err, len(clk.sleeps), tc.want, tc.sleeps)
			}
		})
	}
}

func TestCheckUnreadableMetricDoesNotHoldTheCheckOpenButStaysReported(t *testing.T) {
	ticks := healthySnapshots(4)
	for i := range ticks {
		ticks[i].memory = unobserved[hostMemory](ReasonUnreadable)
	}
	checker, _, clk := newTestChecker(t, ticks)
	got, err := checker.Check(context.Background(), nil)
	if err != nil || got.Status() != StatusUnknown || len(clk.sleeps) != 1 {
		t.Fatalf("status = %s err = %v sleeps = %d, want unknown after one interval", got.Status(), err, len(clk.sleeps))
	}
	if unavailableOf(got)[MetricHostMemory] != ReasonUnreadable {
		t.Fatalf("unavailable = %v", got.Unavailable())
	}
}

func TestCheckHotCPUKeepsSamplingPastAnUnreadableMetric(t *testing.T) {
	ticks := withCPU(healthySnapshots(4), cpuSeries(0.99, 0.99, 0.99))
	for i := range ticks {
		ticks[i].memory = unobserved[hostMemory](ReasonUnreadable)
	}
	checker, _, clk := newTestChecker(t, ticks)
	got, err := checker.Check(context.Background(), nil)
	if err != nil || pressuresOf(got)[MetricHostCPU] != BasisSustained || len(clk.sleeps) != 3 {
		t.Fatalf("pressures = %v err = %v sleeps = %d, want sustained cpu after three intervals", got.Pressures(), err, len(clk.sleeps))
	}
}

func TestCheckSustainedCPUNeedsThreeConsecutiveBusyIntervals(t *testing.T) {
	cases := []struct {
		name   string
		shares []float64
		want   Status
		sleeps int
	}{
		{"three busy", []float64{0.96, 0.99, 0.95}, StatusPressure, 3},
		{"spike then clear", []float64{0.99, 0.99, 0.10}, StatusClear, 3},
		{"cleared after the first busy interval", []float64{0.99, 0.10, 0.99}, StatusClear, 2},
		{"just under threshold", []float64{0.949, 0.949, 0.949}, StatusClear, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ticks := withCPU(healthySnapshots(4), cpuSeries(tc.shares...))
			checker, _, clk := newTestChecker(t, ticks)
			got, err := checker.Check(context.Background(), nil)
			if err != nil || got.Status() != tc.want || len(clk.sleeps) != tc.sleeps {
				t.Fatalf("status = %s err = %v sleeps = %d, want %s with %d", got.Status(), err, len(clk.sleeps), tc.want, tc.sleeps)
			}
			if tc.want == StatusPressure && pressuresOf(got)[MetricHostCPU] != BasisSustained {
				t.Fatalf("pressures = %v, want sustained host cpu", got.Pressures())
			}
		})
	}
}

func TestCheckStopsAtFirstConfirmedPressure(t *testing.T) {
	ticks := withCPU(healthySnapshots(4), cpuSeries(0.99, 0.99, 0.99))
	checker, obs, clk := newTestChecker(t, ticks)
	if _, err := checker.Check(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(obs.targets) != 4 || len(clk.sleeps) != 3 {
		t.Fatalf("observations = %d sleeps = %d, want 4 and 3", len(obs.targets), len(clk.sleeps))
	}
}

func TestCheckCriticalHostMemoryRefusesBeforeAnyWait(t *testing.T) {
	cases := []struct {
		name   string
		memory reading[hostMemory]
	}{
		{"linux below half reserve", availableMemory(gib/2 - 1)},
		{"darwin critical flag", vmMemory(vmPressureCritical)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ticks := healthySnapshots(4)
			ticks[0].memory = tc.memory
			checker, obs, clk := newTestChecker(t, ticks)
			got, err := checker.Check(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if pressuresOf(got)[MetricHostMemory] != BasisCritical {
				t.Fatalf("pressures = %v, want critical host memory", got.Pressures())
			}
			if len(clk.sleeps) != 0 || len(obs.targets) != 0 {
				t.Fatalf("sleeps = %v observations = %d, want the precheck to decide alone", clk.sleeps, len(obs.targets))
			}
		})
	}
}

func TestCheckSustainedLowMemoryNeedsThreeConsecutiveIntervals(t *testing.T) {
	low, ok := availableMemory(gib-1), availableMemory(2*gib)
	warn, normal := vmMemory(vmPressureWarning), vmMemory(vmPressureNormal)
	cases := []struct {
		name    string
		samples [4]reading[hostMemory]
		want    Status
		sleeps  int
	}{
		{"three low intervals", [4]reading[hostMemory]{ok, low, low, low}, StatusPressure, 3},
		{"two low then recovered", [4]reading[hostMemory]{ok, low, low, ok}, StatusClear, 3},
		{"warning flag three intervals", [4]reading[hostMemory]{normal, warn, warn, warn}, StatusPressure, 3},
		{"warning flag twice", [4]reading[hostMemory]{normal, warn, warn, normal}, StatusClear, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ticks := healthySnapshots(4)
			for i := range ticks {
				ticks[i].memory = tc.samples[i]
			}
			checker, _, clk := newTestChecker(t, ticks)
			got, err := checker.Check(context.Background(), nil)
			if err != nil || got.Status() != tc.want || len(clk.sleeps) != tc.sleeps {
				t.Fatalf("status = %s err = %v sleeps = %d, want %s with %d", got.Status(), err, len(clk.sleeps), tc.want, tc.sleeps)
			}
		})
	}
}

func TestCheckUnknownSampleBreaksOnlyItsOwnMetric(t *testing.T) {
	ticks := withCPU(healthySnapshots(4), cpuSeries(0.99, 0.99, 0.99))
	ticks[2].cpu = unobserved[cpuCounters](ReasonUnreadable)
	checker, _, _ := newTestChecker(t, ticks)
	got, err := checker.Check(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() != StatusUnknown {
		t.Fatalf("status = %s, want unknown: a missing reading must not count as busy or idle", got.Status())
	}
	want := map[Metric]Reason{MetricHostCPU: ReasonUnreadable}
	if !reflect.DeepEqual(unavailableOf(got), want) {
		t.Fatalf("unavailable = %v, want only host cpu", got.Unavailable())
	}
}

func TestCheckCounterResetIsUnknownNotZero(t *testing.T) {
	cpus := cpuSeries(0.99, 0.99, 0.99)
	cpus[2] = observed(cpuCounters{busy: 1, total: 2})
	ticks := withCPU(healthySnapshots(4), cpus)
	checker, _, _ := newTestChecker(t, ticks)
	got, err := checker.Check(context.Background(), nil)
	if err != nil || got.Status() != StatusUnknown {
		t.Fatalf("status = %s err = %v, want unknown", got.Status(), err)
	}
	if unavailableOf(got)[MetricHostCPU] != ReasonCounterReset {
		t.Fatalf("unavailable = %v, want counter_reset", got.Unavailable())
	}
}

func TestCheckConfirmedPressureWinsOverAnotherMissingMetric(t *testing.T) {
	ticks := withCPU(healthySnapshots(4), cpuSeries(0.99, 0.99, 0.99))
	for i := range ticks {
		ticks[i].memory = unobserved[hostMemory](ReasonUnreadable)
	}
	checker, _, _ := newTestChecker(t, ticks)
	got, err := checker.Check(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() != StatusPressure {
		t.Fatalf("status = %s, want pressure", got.Status())
	}
	if unavailableOf(got)[MetricHostMemory] != ReasonUnreadable {
		t.Fatalf("unavailable = %v, want the missing memory still reported", got.Unavailable())
	}
}

func TestCheckMissingMetricAloneIsUnknownNotHealthy(t *testing.T) {
	ticks := healthySnapshots(4)
	for i := range ticks {
		ticks[i].memory = unobserved[hostMemory](ReasonUnsupportedPlatform)
	}
	checker, _, _ := newTestChecker(t, ticks)
	got, err := checker.Check(context.Background(), nil)
	if err != nil || got.Status() != StatusUnknown {
		t.Fatalf("status = %s err = %v, want unknown", got.Status(), err)
	}
}

func TestCheckNilResolverLeavesCgroupMetricsUnknown(t *testing.T) {
	ticks := healthySnapshots(4)
	for i := range ticks {
		ticks[i].cgroup = unobserved[cgroupSample](ReasonTargetUnavailable)
	}
	checker, obs, _ := newTestChecker(t, ticks)
	got, err := checker.Check(context.Background(), nil)
	if err != nil || got.Status() != StatusUnknown {
		t.Fatalf("status = %s err = %v, want unknown", got.Status(), err)
	}
	want := map[Metric]Reason{MetricCgroupCPU: ReasonTargetUnavailable, MetricCgroupMemory: ReasonTargetUnavailable}
	if !reflect.DeepEqual(unavailableOf(got), want) {
		t.Fatalf("unavailable = %v, want both cgroup metrics", got.Unavailable())
	}
	for _, target := range obs.targets {
		if target != nil {
			t.Fatalf("observer received target %v, want nil", *target)
		}
	}
}

func TestCheckResolverFailureAndInvalidTargetAreUnavailable(t *testing.T) {
	cases := map[string]resolverFunc{
		"error": func(context.Context) (Target, error) { return testTarget, errResolver },
		"empty": func(context.Context) (Target, error) { return Target{}, nil },
		"no pid or cgroup": func(context.Context) (Target, error) {
			return Target{Identity: "x"}, nil
		},
	}
	for name, resolver := range cases {
		t.Run(name, func(t *testing.T) {
			checker, obs, _ := newTestChecker(t, healthySnapshots(4))
			if _, err := checker.Check(context.Background(), resolver); err != nil {
				t.Fatalf("resolver failure must not fail the check: %v", err)
			}
			if obs.targets[0] != nil {
				t.Fatalf("observer received %v, want nil target", *obs.targets[0])
			}
		})
	}
}

func TestCheckPassesResolvedTargetAndCallsResolverBeforeAndAfter(t *testing.T) {
	checker, obs, _ := newTestChecker(t, healthySnapshots(4))
	resolver := steadyResolver()
	got, err := checker.Check(context.Background(), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 2 {
		t.Fatalf("resolver calls = %d, want before and after", resolver.calls)
	}
	if obs.targets[0] == nil || *obs.targets[0] != testTarget {
		t.Fatalf("observer target = %v, want %v", obs.targets[0], testTarget)
	}
	if got.Scope().TargetIdentity != testTarget.Identity {
		t.Fatalf("scope = %+v, want the target identity", got.Scope())
	}
}

func TestCheckCancellationKeepsCallerSemantics(t *testing.T) {
	t.Run("already cancelled", func(t *testing.T) {
		checker, obs, _ := newTestChecker(t, healthySnapshots(4))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := checker.Check(ctx, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if len(obs.targets) != 0 {
			t.Fatal("a cancelled check must not sample")
		}
	})
	t.Run("cancelled while waiting", func(t *testing.T) {
		checker, _, clk := newTestChecker(t, healthySnapshots(4))
		ctx, cancel := context.WithCancel(context.Background())
		clk.block = true
		clk.onSleep = cancel
		if _, err := checker.Check(ctx, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("caller deadline", func(t *testing.T) {
		checker, _, clk := newTestChecker(t, healthySnapshots(4))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		clk.block = true
		if _, err := checker.Check(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the caller's deadline error", err)
		}
	})
	t.Run("cancelled in resolver", func(t *testing.T) {
		checker, _, _ := newTestChecker(t, healthySnapshots(4))
		ctx, cancel := context.WithCancel(context.Background())
		resolver := resolverFunc(func(context.Context) (Target, error) {
			cancel()
			return Target{}, context.Canceled
		})
		if _, err := checker.Check(ctx, resolver); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

func TestCheckOwnBudgetExpiryIsUnknownWithoutError(t *testing.T) {
	ticks := withCPU(healthySnapshots(4), cpuSeries(0.99, 0.99, 0.99))
	obs := &fakeObserver{ticks: ticks, path: observed("")}
	clk := newFakeClock()
	clk.block = true
	checker := newChecker(obs, clk, 20*time.Millisecond)
	got, err := checker.Check(context.Background(), nil)
	if err != nil {
		t.Fatalf("own budget expiry must not be an error: %v", err)
	}
	if got.Status() != StatusUnknown {
		t.Fatalf("status = %s, want unknown", got.Status())
	}
	want := map[Metric]Reason{MetricHostCPU: ReasonDeadline, MetricHostMemory: ReasonDeadline}
	if !reflect.DeepEqual(unavailableOf(got), want) {
		t.Fatalf("unavailable = %v, want every unfinished metric with deadline", got.Unavailable())
	}
}
