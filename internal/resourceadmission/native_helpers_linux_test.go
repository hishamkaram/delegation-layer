//go:build linux

package resourceadmission

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestAccountedGrowthNeverPassesADecrease(t *testing.T) {
	const added = fixtureCacheBytes
	cases := []struct {
		name          string
		before, after uint64
		want          bool
	}{
		{"exact growth", 10 << 20, 10<<20 + added, true},
		{"growth within slack", 10 << 20, 10<<20 + added - accountingSlackBytes, true},
		{"growth short by more than slack", 10 << 20, 10<<20 + added - accountingSlackBytes - 1, false},
		{"no change", 10 << 20, 10 << 20, false},
		{"decrease", 10 << 20, 10<<20 - pageBytes, false},
		{"decrease to zero", 10 << 20, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountedGrowth(tc.before, tc.after, added); got != tc.want {
				t.Fatalf("accountedGrowth(%d, %d, %d) = %t, want %t", tc.before, tc.after, added, got, tc.want)
			}
		})
	}
}

func TestTickRecorderStampsTheCheckersTimeAfterTheLoadHook(t *testing.T) {
	ticks := withCPU(healthySnapshots(4), cpuSeries(0.99, 0.99, 0.99))
	clk := newFakeClock()
	var hookEnds []time.Time
	rec := &tickRecorder{
		observer: &fakeObserver{ticks: ticks, path: observed(""), untargeted: true},
		base:     clk,
		after: func(int) {
			clk.now = clk.now.Add(sampleInterval / 2)
			hookEnds = append(hookEnds, clk.now)
		},
	}
	got, err := newChecker(rec, rec, checkBudget).Check(context.Background(), nil)
	if err != nil || pressuresOf(got)[MetricHostCPU] != BasisSustained {
		t.Fatalf("pressures = %v err = %v, want the scripted sustained CPU", got.Pressures(), err)
	}
	if len(rec.snaps) != 4 || !reflect.DeepEqual(rec.stamps, hookEnds) {
		t.Fatalf("snaps %d stamps %v, want one stamp per tick taken after its hook %v", len(rec.snaps), rec.stamps, hookEnds)
	}
	for i, d := range rec.intervals() {
		if want := hookEnds[i+1].Sub(hookEnds[i]); d != want {
			t.Fatalf("interval %d = %v, want the checker's %v", i, d, want)
		}
	}
}

func TestHotStreakCountsOnlyConsecutiveSaturatedIntervals(t *testing.T) {
	hot, clear, unknown := hotOutcome, clearOutcome, unknownOutcome(ReasonDeadline)
	cases := []struct {
		name     string
		outcomes []outcome
		want     int
	}{
		{"none", nil, 0},
		{"three in a row", []outcome{hot, hot, hot}, 3},
		{"three in total split by clear", []outcome{hot, hot, clear, hot}, 2},
		{"three in total split by unknown", []outcome{hot, unknown, hot, hot}, 2},
		{"longest run wins", []outcome{hot, clear, hot, hot, hot}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hotStreak(tc.outcomes); got != tc.want {
				t.Fatalf("hotStreak = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCPUInvalidationIsOnlyTheCanonicalWithdrawals(t *testing.T) {
	at := time.Unix(1_000, 0)
	for reason, want := range map[Reason]bool{
		ReasonDeadline: true, ReasonTargetChanged: true, ReasonTargetUnavailable: true,
		ReasonUnreadable: false, ReasonPressureUnavailable: false,
	} {
		got := NewAssessment(at, Scope{}, Findings{Unavailable: []Unavailable{{Metric: MetricCgroupCPU, Reason: reason}}})
		if cpuInvalidated(got) != want {
			t.Fatalf("cgroup CPU %s: invalidated %t, want %t", reason, !want, want)
		}
	}
	other := NewAssessment(at, Scope{}, Findings{Unavailable: []Unavailable{{Metric: MetricCgroupMemory, Reason: ReasonDeadline}}})
	if cpuInvalidated(other) {
		t.Fatal("a cgroup memory deadline must not withdraw cgroup CPU")
	}
}

func TestCPUIntervalLogNamesUnavailableReadingsWithoutPanicking(t *testing.T) {
	level := func(usage uint64) cgroupLevel { return quotaLevel(containerQuotaCPUs, usage) }
	cases := map[string][2]reading[cgroupSample]{
		"expired reading":   {cgroupAt(observed(uint64(0)), level(0)), unobserved[cgroupSample](ReasonDeadline)},
		"no level":          {observed(cgroupSample{path: cgroupPathA}), observed(cgroupSample{path: cgroupPathA})},
		"cpu unreadable":    {cgroupAt(observed(uint64(0)), cgroupLevel{cpu: unobserved[levelCPU](ReasonUnreadable)}), cgroupAt(observed(uint64(0)), level(1))},
		"counter backwards": {cgroupAt(observed(uint64(0)), level(2*cpuSecond)), cgroupAt(observed(uint64(0)), level(cpuSecond))},
	}
	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			if used, missing := cgroupCPUUsage(pair[0], pair[1]); missing == "" {
				t.Fatalf("usage %d, want the reading named unavailable", used)
			}
			at := time.Unix(1_000, 0)
			rec := &tickRecorder{
				snaps:  []snapshot{{cgroup: pair[0]}, {cgroup: pair[1]}},
				stamps: []time.Time{at, at.Add(sampleInterval)},
			}
			logCPUIntervals(t, 1, rec)
		})
	}
	used, missing := cgroupCPUUsage(cgroupAt(observed(uint64(0)), level(cpuSecond)), cgroupAt(observed(uint64(0)), level(cpuSecond+500_000)))
	if missing != "" || used != 500_000 {
		t.Fatalf("usage %d missing %q, want 500000us", used, missing)
	}
}
