//go:build linux

package resourceadmission

import (
	"context"
	"testing"
	"time"
)

// cpuMaxLevel is a level whose quota comes from a raw cpu.max text, so the
// capacity is exactly what the production parser yields.
func cpuMaxLevel(t *testing.T, cpuMax string, cpusetCPUs int, usageUS uint64) reading[levelCPU] {
	t.Helper()
	cpus, hasQuota, err := parseCPUMax(cpuMax)
	if err != nil {
		t.Fatalf("parseCPUMax(%q): %v", cpuMax, err)
	}
	return observed(levelCPU{quotaCPUs: cpus, hasQuota: hasQuota, cpusetCPUs: cpusetCPUs, usageUS: usageUS})
}

func TestLevelCPUOutcomeEffectiveCapacityChangeIsUnknown(t *testing.T) {
	cases := []struct {
		name               string
		prevMax, curMax    string
		prevSet, curSet    int
		usageUS            uint64
		wantState          outcomeState
		wantTargetChanged  bool
		describeOldVerdict string
	}{
		{
			name: "quota lowered at the interval end", prevMax: "200000 100000", curMax: "50000 100000",
			usageUS: cpuSecond, wantTargetChanged: true,
			describeOldVerdict: "half the old capacity was judged 200% of the new one",
		},
		{
			name: "quota raised at the interval end", prevMax: "50000 100000", curMax: "200000 100000",
			usageUS: cpuSecond / 2, wantTargetChanged: true,
			describeOldVerdict: "a saturated old quota was judged 25% of the new one",
		},
		{
			name: "cpuset becomes the tighter bound", prevMax: "max 100000", curMax: "max 100000", prevSet: 4, curSet: 1,
			usageUS: 2 * cpuSecond, wantTargetChanged: true,
			describeOldVerdict: "half of four CPUs was judged 200% of one",
		},
		{
			name: "period and quota change proportionally", prevMax: "33333 100000", curMax: "66666 200000",
			usageUS: 330_000, wantState: outcomeHot,
		},
		{
			name: "cpuset changes while the quota stays the bottleneck", prevMax: "50000 100000", curMax: "50000 100000", prevSet: 8, curSet: 2,
			usageUS: 500_000, wantState: outcomeHot,
		},
		{
			name: "unchanged capacity below threshold", prevMax: "50000 100000", curMax: "50000 100000",
			usageUS: 400_000, wantState: outcomeClear,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := cpuMaxLevel(t, tc.prevMax, tc.prevSet, cpuSecond)
			cur := cpuMaxLevel(t, tc.curMax, tc.curSet, cpuSecond+tc.usageUS)
			got := levelCPUOutcome(prev, cur, time.Second)
			if tc.wantTargetChanged {
				if got != unknownOutcome(ReasonTargetChanged) {
					t.Fatalf("outcome = %+v, want target_changed (%s)", got, tc.describeOldVerdict)
				}
				return
			}
			if got.state != tc.wantState || got.reason != "" {
				t.Fatalf("outcome = %+v, want state %d with no reason", got, tc.wantState)
			}
		})
	}
}

// quotaTicks builds one cgroup reading per tick from a quota in CPUs and
// cumulative usage.
func quotaTicks(quotas []float64, usage []uint64) []snapshot {
	samples := make([]reading[cgroupSample], len(quotas))
	for i := range samples {
		samples[i] = cgroupAt(observed(uint64(0)), quotaLevel(quotas[i], usage[i]))
	}
	return withCgroup(healthySnapshots(len(quotas)), samples...)
}

func TestCheckCannotConfirmCPUPressureAcrossACapacityChange(t *testing.T) {
	// Two CPUs, then 0.5 from the first interval's end: the first interval
	// used half the old quota, and only the next two saturate the new one.
	lowered := quotaTicks([]float64{2, 0.5, 0.5, 0.5}, []uint64{0, cpuSecond, 3 * cpuSecond / 2, 2 * cpuSecond})
	checker, _, _ := newTestChecker(t, lowered)
	got, err := checker.Check(context.Background(), steadyResolver())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status() == StatusPressure {
		t.Fatalf("pressures = %v, want no confirmation from an interval measured against a changed quota", got.Pressures())
	}
	if unavailableOf(got)[MetricCgroupCPU] != ReasonTargetChanged {
		t.Fatalf("unavailable = %v, want cgroup cpu target_changed", got.Unavailable())
	}

	stable := quotaTicks([]float64{0.5, 0.5, 0.5, 0.5}, []uint64{0, cpuSecond / 2, cpuSecond, 3 * cpuSecond / 2})
	checker, _, _ = newTestChecker(t, stable)
	got, err = checker.Check(context.Background(), steadyResolver())
	if err != nil || pressuresOf(got)[MetricCgroupCPU] != BasisSustained {
		t.Fatalf("pressures = %v err = %v, want a later check with three stable saturated intervals to refuse", got.Pressures(), err)
	}
}
