package resourceadmission

import (
	"math"
	"testing"
	"time"
)

func TestReserveBytesIsTheSmallerOfOneGiBAndTenPercent(t *testing.T) {
	cases := []struct{ total, want uint64 }{
		{0, 0},
		{4 * gib, 4 * gib / 10},
		{10 * gib, gib},
		{64 * gib, gib},
	}
	for _, tc := range cases {
		if got := reserveBytes(tc.total); got != tc.want {
			t.Errorf("reserveBytes(%d) = %d, want %d", tc.total, got, tc.want)
		}
	}
}

func TestClassifyAvailableBoundaries(t *testing.T) {
	const total = 16 * gib
	cases := []struct {
		name      string
		total     uint64
		available uint64
		want      outcomeState
	}{
		{"at reserve is clear", total, gib, outcomeClear},
		{"one byte under reserve is hot", total, gib - 1, outcomeHot},
		{"at half reserve is hot", total, gib / 2, outcomeHot},
		{"one byte under half reserve is critical", total, gib/2 - 1, outcomeCritical},
		{"small host uses ten percent", 2 * gib, 2*gib/10 - 1, outcomeHot},
		{"zero total is not judged", 0, 0, outcomeUnknown},
		{"available above total is invalid", total, total + 1, outcomeUnknown},
	}
	for _, tc := range cases {
		if got := classifyAvailable(tc.total, tc.available).state; got != tc.want {
			t.Errorf("%s: state = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestClassifyVMPressure(t *testing.T) {
	cases := []struct {
		level vmPressure
		want  outcomeState
	}{
		{vmPressureNormal, outcomeClear},
		{vmPressureWarning, outcomeHot},
		{vmPressureCritical, outcomeCritical},
		{0, outcomeUnknown},
		{3, outcomeUnknown},
		{8, outcomeUnknown},
	}
	for _, tc := range cases {
		if got := classifyVMPressure(tc.level).state; got != tc.want {
			t.Errorf("level %d: state = %d, want %d", tc.level, got, tc.want)
		}
	}
}

func TestHostMemoryOutcomeIgnoresAvailableMemoryOnMac(t *testing.T) {
	r := observed(hostMemory{source: memorySourceVMPressure, total: 16 * gib, available: 0, vm: vmPressureNormal})
	if got := hostMemoryOutcome(r).state; got != outcomeClear {
		t.Fatalf("state = %d, want clear: available memory is not a macOS rule", got)
	}
}

func TestHostCPUOutcome(t *testing.T) {
	at := func(busy, total float64) reading[cpuCounters] { return observed(cpuCounters{busy: busy, total: total}) }
	cases := []struct {
		name      string
		prev, cur reading[cpuCounters]
		want      outcomeState
		reason    Reason
	}{
		{"busy at threshold", at(0, 0), at(95, 100), outcomeHot, ""},
		{"busy under threshold", at(10, 20), at(104, 120), outcomeClear, ""},
		{"idle", at(5, 10), at(5, 110), outcomeClear, ""},
		{"zero total delta", at(5, 10), at(5, 10), outcomeUnknown, ReasonNoElapsedTime},
		{"total went backwards", at(5, 10), at(5, 9), outcomeUnknown, ReasonCounterReset},
		{"busy went backwards", at(5, 10), at(4, 20), outcomeUnknown, ReasonCounterReset},
		{"current missing", at(0, 0), unobserved[cpuCounters](ReasonMalformed), outcomeUnknown, ReasonMalformed},
		{"baseline missing", unobserved[cpuCounters](ReasonUnreadable), at(1, 1), outcomeUnknown, ReasonUnreadable},
		{"zero value reading is not healthy", reading[cpuCounters]{}, at(1, 1), outcomeUnknown, ReasonUnreadable},
		{"skipped", notApplicable[cpuCounters](), notApplicable[cpuCounters](), outcomeSkipped, ""},
		{"busy above total", at(0, 0), at(11, 10), outcomeUnknown, ReasonMalformed},
		{"baseline busy above total", at(11, 10), at(12, 20), outcomeUnknown, ReasonMalformed},
		{"busy delta above total delta", at(0, 10), at(9, 12), outcomeUnknown, ReasonMalformed},
		{"nan", at(0, 0), at(math.NaN(), 10), outcomeUnknown, ReasonMalformed},
		{"infinite total", at(0, 0), at(5, math.Inf(1)), outcomeUnknown, ReasonMalformed},
		{"negative", at(0, 0), at(-1, 10), outcomeUnknown, ReasonMalformed},
	}
	for _, tc := range cases {
		got := hostCPUOutcome(tc.prev, tc.cur)
		if got.state != tc.want || got.reason != tc.reason {
			t.Errorf("%s: outcome = %+v, want state %d reason %q", tc.name, got, tc.want, tc.reason)
		}
	}
}

func TestPSIOutcomeNeedsAPositiveElapsedShare(t *testing.T) {
	prev, cur := observed(uint64(1_000)), observed(uint64(1_000+onePercentStall))
	if got := psiOutcome(prev, cur, time.Second).state; got != outcomeHot {
		t.Fatalf("state = %d, want hot at exactly 1%%", got)
	}
	if got := psiOutcome(prev, cur, 2*time.Second).state; got != outcomeClear {
		t.Fatalf("state = %d, want clear when the same stall spans a longer interval", got)
	}
	if got := psiOutcome(cur, prev, time.Second); got.reason != ReasonCounterReset {
		t.Fatalf("outcome = %+v, want counter_reset", got)
	}
}

func TestCgroupPairRefusesMismatchedOrInstantSamples(t *testing.T) {
	a := cgroupAt(observed(uint64(0)), memoryLevel(gib, 0))
	other := cgroupAt(observed(uint64(0)), memoryLevel(gib, 0))
	moved := other.value
	moved.path = "/elsewhere"
	deeper := other.value
	deeper.levels = append(append([]cgroupLevel(nil), deeper.levels...), memoryLevel(gib, 0))
	cases := []struct {
		name      string
		prev, cur reading[cgroupSample]
		elapsed   time.Duration
		want      Reason
	}{
		{"different path", a, observed(moved), time.Second, ReasonTargetChanged},
		{"different depth", a, observed(deeper), time.Second, ReasonTargetChanged},
		{"no elapsed time", a, other, 0, ReasonNoElapsedTime},
		{"current unobserved", a, unobserved[cgroupSample](ReasonScopeUnresolved), time.Second, ReasonScopeUnresolved},
		{"baseline unobserved", unobserved[cgroupSample](ReasonCgroupUnsupported), a, time.Second, ReasonCgroupUnsupported},
	}
	for _, tc := range cases {
		got, ok := cgroupPair(tc.prev, tc.cur, tc.elapsed)
		if ok || got.reason != tc.want {
			t.Errorf("%s: outcome = %+v ok = %v, want reason %q", tc.name, got, ok, tc.want)
		}
	}
}

func TestSubMicrosecondIntervalsAreNotJudged(t *testing.T) {
	a := cgroupAt(observed(uint64(0)), memoryLevel(gib, 0), quotaLevel(1, 0))
	b := cgroupAt(observed(uint64(10_000)), memoryLevel(gib, 0), quotaLevel(1, 1000))
	for _, elapsed := range []time.Duration{0, -time.Second, 999 * time.Nanosecond} {
		if got := cgroupCPUOutcome(a, b, elapsed); got.reason != ReasonNoElapsedTime {
			t.Errorf("cpu at %v = %+v, want no_elapsed_time", elapsed, got)
		}
		if got := cgroupMemoryOutcome(a, b, elapsed); got.reason != ReasonNoElapsedTime {
			t.Errorf("memory at %v = %+v, want no_elapsed_time", elapsed, got)
		}
		if got := psiOutcome(a.value.psiStallUS, b.value.psiStallUS, elapsed); got.reason != ReasonNoElapsedTime {
			t.Errorf("psi at %v = %+v, want no_elapsed_time", elapsed, got)
		}
	}
}

func TestZeroQuotaCapacityIsInvalidNotInfiniteBusy(t *testing.T) {
	prev := observed(levelCPU{hasQuota: true, quotaCPUs: 0, usageUS: 0})
	cur := observed(levelCPU{hasQuota: true, quotaCPUs: 0, usageUS: 500})
	if got := levelCPUOutcome(prev, cur, time.Second); got.reason != ReasonMalformed {
		t.Fatalf("outcome = %+v, want malformed", got)
	}
}

func TestCPUCapacityIsTheTighterOfQuotaAndCpuset(t *testing.T) {
	cases := []struct {
		name string
		in   levelCPU
		want float64
		ok   bool
	}{
		{"unconstrained", levelCPU{}, 0, false},
		{"quota only", levelCPU{quotaCPUs: 1.5, hasQuota: true}, 1.5, true},
		{"cpuset only", levelCPU{cpusetCPUs: 4}, 4, true},
		{"quota tighter", levelCPU{quotaCPUs: 2, hasQuota: true, cpusetCPUs: 8}, 2, true},
		{"cpuset tighter", levelCPU{quotaCPUs: 8, hasQuota: true, cpusetCPUs: 2}, 2, true},
	}
	for _, tc := range cases {
		got, ok := cpuCapacity(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: capacity = %v %v, want %v %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestHeadroomLowUsesTheLimitReserve(t *testing.T) {
	cases := []struct {
		name         string
		limit, usage uint64
		want         bool
	}{
		{"big limit, headroom at 1 GiB", 16 * gib, 15 * gib, false},
		{"big limit, headroom just under 1 GiB", 16 * gib, 15*gib + 1, true},
		{"small limit uses ten percent", 2 * gib, 2*gib - (2*gib)/10, false},
		{"small limit just below ten percent", 2 * gib, 2*gib - (2*gib)/10 + 1, true},
		{"usage above limit saturates", gib, 2 * gib, true},
		{"idle level", 16 * gib, 0, false},
	}
	for _, tc := range cases {
		if got := headroomLow(tc.limit, tc.usage); got != tc.want {
			t.Errorf("%s: headroomLow = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLevelCPUOutcomeConstraintAppearingMidRunIsUnknown(t *testing.T) {
	prev := observed(levelCPU{})
	cur := observed(levelCPU{quotaCPUs: 1, hasQuota: true, usageUS: 5})
	if got := levelCPUOutcome(prev, cur, time.Second); got.reason != ReasonTargetChanged {
		t.Fatalf("outcome = %+v, want target_changed", got)
	}
}
