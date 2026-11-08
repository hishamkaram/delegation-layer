package resourceadmission

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestGuardSelectionOnlyReportsEnabledPressure(t *testing.T) {
	cases := []struct {
		name     string
		policy   GuardPolicy
		want     []Pressure
		disabled []string
	}{
		{"default", GuardPolicy{}, []Pressure{{MetricHostCPU, BasisSustained}, {MetricHostMemory, BasisSustained}}, nil},
		{"CPU off", GuardPolicy{DisableCPU: true}, []Pressure{{MetricHostMemory, BasisSustained}}, []string{"cpu"}},
		{"memory off", GuardPolicy{DisableMemory: true}, []Pressure{{MetricHostCPU, BasisSustained}}, []string{"memory"}},
		{"both off", GuardPolicy{DisableCPU: true, DisableMemory: true}, nil, []string{"cpu", "memory"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ticks := withCPU(healthySnapshots(4), cpuSeries(.99, .99, .99))
			for i := range ticks {
				ticks[i].memory = availableMemory(gib - 1)
			}
			checker, _, _ := newTestChecker(t, ticks)
			checker.policy = tc.policy
			got, err := checker.Check(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Pressures(), tc.want) || !reflect.DeepEqual(got.DisabledGuards(), tc.disabled) || got.Policy() != tc.policy {
				t.Fatalf("policy=%+v pressures=%v disabled=%v", got.Policy(), got.Pressures(), got.DisabledGuards())
			}
			if len(got.Unavailable()) != 0 {
				t.Fatalf("disabled metrics unavailable: %v", got.Unavailable())
			}
		})
	}
}

func TestBothDisabledSkipsScopeAndHonorsCancellation(t *testing.T) {
	checker, obs, clk := newTestChecker(t, nil)
	checker.policy = GuardPolicy{DisableCPU: true, DisableMemory: true}
	resolver := resolverFunc(func(context.Context) (Target, error) {
		t.Fatal("disabled guards resolved target")
		return Target{}, nil
	})
	if _, err := checker.Check(t.Context(), resolver); err != nil {
		t.Fatal(err)
	}
	if _, err := checker.Scope(t.Context(), resolver); err != nil {
		t.Fatal(err)
	}
	if obs.memoryCalls != 0 || obs.membershipCalls != 0 || len(obs.targets) != 0 || len(clk.sleeps) != 0 {
		t.Fatal("disabled guards observed resources")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := checker.Check(ctx, resolver); !errors.Is(err, context.Canceled) {
		t.Fatalf("Check cancellation=%v", err)
	}
	if _, err := checker.Scope(ctx, resolver); !errors.Is(err, context.Canceled) {
		t.Fatalf("Scope cancellation=%v", err)
	}
}

func TestDisabledHostReadersAreNotInvoked(t *testing.T) {
	for _, policy := range []GuardPolicy{{DisableCPU: true}, {DisableMemory: true}} {
		cpuReads, memoryReads := 0, 0
		observer := composedObserver{
			cpu:    func(context.Context) reading[cpuCounters] { cpuReads++; return observed(cpuCounters{}) },
			memory: func(context.Context) reading[hostMemory] { memoryReads++; return availableMemory(2 * gib) },
			cgroup: func(*Target) reading[cgroupSample] { return notApplicable[cgroupSample]() }, cgroupPath: func(Target) reading[string] { return notApplicable[string]() },
		}.withPolicy(policy)
		observer.observe(t.Context(), nil)
		observer.hostMemory(t.Context())
		if cpuReads != boolCount(!policy.DisableCPU) || memoryReads != 2*boolCount(!policy.DisableMemory) {
			t.Fatalf("policy=%+v reads CPU=%d memory=%d", policy, cpuReads, memoryReads)
		}
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestDisabledTracksDoNotBecomeUnavailableBeforeBaseline(t *testing.T) {
	checker, _, _ := newTestChecker(t, nil)
	checker.policy = GuardPolicy{DisableCPU: true}
	r := run{checker: checker, eval: evaluator{policy: checker.policy}}
	r.eval.expect(r.unsampled())
	r.eval.expire(ReasonDeadline)
	got := checker.assessment(Scope{}, r.eval.findings())
	for _, u := range got.Unavailable() {
		if u.Metric == MetricHostCPU || u.Metric == MetricCgroupCPU {
			t.Fatalf("disabled CPU deadline warning=%v", got.Unavailable())
		}
	}
	if len(got.Unavailable()) != 2 {
		t.Fatalf("enabled memory deadline findings=%v", got.Unavailable())
	}
}

func TestGuardSelectionFiltersSharedCgroupReadAndAdvisory(t *testing.T) {
	samples := make([]reading[cgroupSample], 4)
	for i := range samples {
		level := memoryLevel(2*gib, 2*gib-100<<20)
		level.cpu = observed(levelCPU{quotaCPUs: 1, hasQuota: true, usageUS: uint64(i) * cpuSecond})
		memory := level.memory.value
		memory.high = limit{bytes: gib, set: true}
		level.memory = observed(memory)
		samples[i] = cgroupAt(observed(uint64(i)*onePercentStall), level)
	}
	cases := []struct {
		policy     GuardPolicy
		metric     Metric
		advisories int
	}{
		{GuardPolicy{DisableCPU: true}, MetricCgroupMemory, 1},
		{GuardPolicy{DisableMemory: true}, MetricCgroupCPU, 0},
	}
	for _, tc := range cases {
		checker, _, _ := newTestChecker(t, withCgroup(healthySnapshots(4), samples...))
		checker.policy = tc.policy
		got, err := checker.Check(t.Context(), steadyResolver())
		if err != nil || !reflect.DeepEqual(got.Pressures(), []Pressure{{Metric: tc.metric, Basis: BasisSustained}}) {
			t.Fatalf("policy=%+v findings=%v err=%v", tc.policy, got.Pressures(), err)
		}
		if len(got.Advisories()) != tc.advisories || len(got.Unavailable()) != 0 {
			t.Fatalf("policy=%+v advisories=%v unavailable=%v", tc.policy, got.Advisories(), got.Unavailable())
		}
	}
}

func TestGuardSelectionControlsCriticalMemoryPrecheck(t *testing.T) {
	for _, cpuOff := range []bool{false, true} {
		ticks := healthySnapshots(4)
		ticks[0].memory = availableMemory(gib/2 - 1)
		checker, obs, clk := newTestChecker(t, ticks)
		checker.policy = GuardPolicy{DisableCPU: cpuOff, DisableMemory: !cpuOff}
		got, err := checker.Check(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if cpuOff {
			if pressuresOf(got)[MetricHostMemory] != BasisCritical || len(clk.sleeps) != 0 {
				t.Fatalf("enabled critical memory did not refuse immediately: %v", got.Pressures())
			}
		} else if got.Status() != StatusClear || obs.memoryCalls != 0 {
			t.Fatalf("disabled memory precheck ran: %s calls%d", got.Status(), obs.memoryCalls)
		}
	}
}
