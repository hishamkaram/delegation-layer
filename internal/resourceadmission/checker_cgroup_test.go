package resourceadmission

import (
	"context"
	"reflect"
	"testing"
)

const (
	cgroupPathA = "/system.slice/job.scope"
	// onePercentStall is 1% of a one-second interval in microseconds.
	onePercentStall = 10_000
	cpuSecond       = 1_000_000
)

// memoryLevel is a level with a hard limit and a working-set estimate.
func memoryLevel(maxBytes, workingSet uint64) cgroupLevel {
	return cgroupLevel{
		path:   "level",
		memory: observed(levelMemory{max: limit{bytes: maxBytes, set: true}, workingSet: workingSet}),
		cpu:    observed(levelCPU{}),
	}
}

// quotaLevel is a level with a CPU quota and cumulative usage.
func quotaLevel(cpus float64, usageUS uint64) cgroupLevel {
	return cgroupLevel{
		path:   "level",
		memory: observed(levelMemory{}),
		cpu:    observed(levelCPU{quotaCPUs: cpus, hasQuota: true, usageUS: usageUS}),
	}
}

func cgroupAt(psi reading[uint64], levels ...cgroupLevel) reading[cgroupSample] {
	return observed(cgroupSample{path: cgroupPathA, psiStallUS: psi, levels: levels})
}

// withCgroup sets one cgroup reading per tick.
func withCgroup(ticks []snapshot, samples ...reading[cgroupSample]) []snapshot {
	for i := range ticks {
		ticks[i].cgroup = samples[i]
	}
	return ticks
}

func psiSeries(step uint64) []reading[uint64] {
	out := make([]reading[uint64], 4)
	for i := range out {
		out[i] = observed(uint64(i) * step)
	}
	return out
}

func memoryTicks(psi []reading[uint64], workingSet func(tick int) uint64) []snapshot {
	samples := make([]reading[cgroupSample], 4)
	for i := range samples {
		samples[i] = cgroupAt(psi[i], memoryLevel(2*gib, workingSet(i)))
	}
	return withCgroup(healthySnapshots(4), samples...)
}

func constant(v uint64) func(int) uint64 { return func(int) uint64 { return v } }

func TestCgroupMemoryNeedsLowHeadroomAndTargetPSIForThreeIntervals(t *testing.T) {
	cases := []struct {
		name  string
		psi   []reading[uint64]
		ws    func(int) uint64
		want  Status
		basis Basis
	}{
		{"low headroom and 1 percent stall", psiSeries(onePercentStall), constant(2*gib - 100<<20), StatusPressure, BasisSustained},
		{"low headroom without stall", psiSeries(0), constant(2*gib - 100<<20), StatusClear, ""},
		{"stall just under 1 percent", psiSeries(onePercentStall - 1), constant(2*gib - 100<<20), StatusClear, ""},
		{"stall with ample headroom (cache-heavy)", psiSeries(onePercentStall), constant(gib / 4), StatusClear, ""},
		{"headroom recovers on the last interval", psiSeries(onePercentStall), func(i int) uint64 {
			if i == 3 {
				return gib / 4
			}
			return 2*gib - 100<<20
		}, StatusClear, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checker, _, _ := newTestChecker(t, memoryTicks(tc.psi, tc.ws))
			got, err := checker.Check(context.Background(), steadyResolver())
			if err != nil || got.Status() != tc.want {
				t.Fatalf("status = %s err = %v, want %s", got.Status(), err, tc.want)
			}
			if tc.want == StatusPressure && pressuresOf(got)[MetricCgroupMemory] != tc.basis {
				t.Fatalf("pressures = %v, want sustained cgroup memory", got.Pressures())
			}
		})
	}
}

func missingPSI() []reading[uint64] {
	psi := make([]reading[uint64], 4)
	for i := range psi {
		psi[i] = unobserved[uint64](ReasonPressureUnavailable)
	}
	return psi
}

func checkMemory(t *testing.T, ticks []snapshot) Assessment {
	t.Helper()
	checker, _, _ := newTestChecker(t, ticks)
	got, err := checker.Check(context.Background(), steadyResolver())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCgroupMemoryLowHeadroomWithoutPSIIsUnknown(t *testing.T) {
	got := checkMemory(t, memoryTicks(missingPSI(), constant(2*gib-100<<20)))
	if got.Status() != StatusUnknown || unavailableOf(got)[MetricCgroupMemory] != ReasonPressureUnavailable {
		t.Fatalf("status = %s unavailable = %v, want unknown pressure_unavailable", got.Status(), got.Unavailable())
	}
}

func TestCgroupMemoryAmpleHeadroomWithoutPSIStaysUnknown(t *testing.T) {
	got := checkMemory(t, memoryTicks(missingPSI(), constant(gib/4)))
	if got.Status() != StatusUnknown || unavailableOf(got)[MetricCgroupMemory] != ReasonPressureUnavailable {
		t.Fatalf("status = %s unavailable = %v, want unknown: a constrained level with no PSI is a coverage gap",
			got.Status(), got.Unavailable())
	}
}

func TestCgroupMemoryUnconstrainedDoesNotNeedPSI(t *testing.T) {
	samples := make([]reading[cgroupSample], 4)
	psi := missingPSI()
	for i := range samples {
		samples[i] = cgroupAt(psi[i], cgroupLevel{path: "level", memory: observed(levelMemory{}), cpu: observed(levelCPU{})})
	}
	got := checkMemory(t, withCgroup(healthySnapshots(4), samples...))
	if got.Status() != StatusClear {
		t.Fatalf("status = %s, want clear", got.Status())
	}
}

func TestCgroupMemoryUnreadableLevelWithHealthyPSIStaysUnknown(t *testing.T) {
	samples := make([]reading[cgroupSample], 4)
	stall := psiSeries(0)
	for i := range samples {
		samples[i] = cgroupAt(stall[i], cgroupLevel{path: "level", memory: unobserved[levelMemory](ReasonMalformed), cpu: observed(levelCPU{})})
	}
	got := checkMemory(t, withCgroup(healthySnapshots(4), samples...))
	if unavailableOf(got)[MetricCgroupMemory] != ReasonMalformed {
		t.Fatalf("unavailable = %v, want malformed retained", got.Unavailable())
	}
}

func TestHiddenAncestorsKeepACoverageWarningWithoutRefusing(t *testing.T) {
	samples := make([]reading[cgroupSample], 4)
	psi := psiSeries(0)
	for i := range samples {
		sample := cgroupAt(psi[i], memoryLevel(64*gib, gib), quotaLevel(2, 0))
		sample.value.hiddenAncestors = true
		samples[i] = sample
	}
	checker, _, _ := newTestChecker(t, withCgroup(healthySnapshots(4), samples...))
	got, err := checker.Check(context.Background(), steadyResolver())
	if err != nil || got.Status() != StatusUnknown {
		t.Fatalf("status = %s err = %v, want unknown", got.Status(), err)
	}
	want := map[Metric]Reason{MetricCgroupCPU: ReasonScopeUnresolved, MetricCgroupMemory: ReasonScopeUnresolved}
	if !reflect.DeepEqual(unavailableOf(got), want) {
		t.Fatalf("unavailable = %v, want both cgroup metrics", got.Unavailable())
	}
}

func TestUntargetedPlatformNeverCallsTheResolver(t *testing.T) {
	checker, obs, _ := newTestChecker(t, healthySnapshots(4))
	obs.untargeted = true
	resolver := resolverFunc(func(context.Context) (Target, error) {
		t.Fatal("the resolver must not be called on a platform without target metrics")
		return Target{}, errResolver
	})
	got, err := checker.Check(context.Background(), resolver)
	if err != nil || got.Status() != StatusClear || got.Scope() != (Scope{}) {
		t.Fatalf("status = %s err = %v scope = %+v, want clear without a target", got.Status(), err, got.Scope())
	}
	if scope, err := checker.Scope(context.Background(), resolver); err != nil || scope != (Scope{}) {
		t.Fatalf("scope = %+v err = %v, want zero", scope, err)
	}
}

func TestCgroupMemoryAnyConstrainedAncestorCounts(t *testing.T) {
	samples := make([]reading[cgroupSample], 4)
	psi := psiSeries(onePercentStall)
	for i := range samples {
		samples[i] = cgroupAt(psi[i], memoryLevel(64*gib, gib), memoryLevel(2*gib, 2*gib-100<<20))
	}
	checker, _, _ := newTestChecker(t, withCgroup(healthySnapshots(4), samples...))
	got, err := checker.Check(context.Background(), steadyResolver())
	if err != nil || pressuresOf(got)[MetricCgroupMemory] != BasisSustained {
		t.Fatalf("pressures = %v err = %v, want the tight ancestor to count", got.Pressures(), err)
	}
}

func TestCgroupMemoryHighIsAdvisoryOnly(t *testing.T) {
	level := cgroupLevel{
		path: "level",
		memory: observed(levelMemory{
			high:       limit{bytes: 2 * gib, set: true},
			workingSet: 2*gib - 100<<20,
		}),
		cpu: observed(levelCPU{}),
	}
	samples := make([]reading[cgroupSample], 4)
	psi := psiSeries(onePercentStall)
	for i := range samples {
		samples[i] = cgroupAt(psi[i], level)
	}
	checker, _, _ := newTestChecker(t, withCgroup(healthySnapshots(4), samples...))
	got, err := checker.Check(context.Background(), steadyResolver())
	if err != nil || got.Status() != StatusClear {
		t.Fatalf("status = %s err = %v, want clear: memory.high never refuses", got.Status(), err)
	}
	if !reflect.DeepEqual(got.Advisories(), []Advisory{AdvisoryMemoryHighNear}) {
		t.Fatalf("advisories = %v, want memory.high advisory", got.Advisories())
	}
}

func TestCgroupCPUPairsAncestorUsageWithAncestorCapacity(t *testing.T) {
	cases := []struct {
		name         string
		usagePerTick uint64
		want         Status
	}{
		{"siblings saturate the quota", 1_960_000, StatusPressure},
		{"exactly at threshold", 1_900_000, StatusPressure},
		{"below threshold", 1_899_999, StatusClear},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			samples := make([]reading[cgroupSample], 4)
			for i := range samples {
				samples[i] = cgroupAt(observed(uint64(0)), quotaLevel(2, uint64(i)*tc.usagePerTick))
			}
			checker, _, _ := newTestChecker(t, withCgroup(healthySnapshots(4), samples...))
			got, err := checker.Check(context.Background(), steadyResolver())
			if err != nil || got.Status() != tc.want {
				t.Fatalf("status = %s err = %v, want %s", got.Status(), err, tc.want)
			}
			if tc.want == StatusPressure && pressuresOf(got)[MetricCgroupCPU] != BasisSustained {
				t.Fatalf("pressures = %v, want cgroup cpu", got.Pressures())
			}
		})
	}
}

func TestCgroupUsageResetIsUnknown(t *testing.T) {
	usage := []uint64{0, 2 * cpuSecond, 1, 4 * cpuSecond}
	samples := make([]reading[cgroupSample], 4)
	for i := range samples {
		samples[i] = cgroupAt(observed(uint64(0)), quotaLevel(2, usage[i]))
	}
	checker, _, _ := newTestChecker(t, withCgroup(healthySnapshots(4), samples...))
	got, err := checker.Check(context.Background(), steadyResolver())
	if err != nil || unavailableOf(got)[MetricCgroupCPU] != ReasonCounterReset {
		t.Fatalf("unavailable = %v err = %v, want counter_reset", got.Unavailable(), err)
	}
}

func TestCgroupPathChangeBetweenTicksInvalidatesTheSequence(t *testing.T) {
	psi := psiSeries(onePercentStall)
	samples := make([]reading[cgroupSample], 4)
	for i := range samples {
		samples[i] = cgroupAt(psi[i], memoryLevel(2*gib, 2*gib-100<<20))
	}
	moved := samples[2].value
	moved.path = "/other.scope"
	samples[2] = observed(moved)
	checker, _, _ := newTestChecker(t, withCgroup(healthySnapshots(4), samples...))
	got, err := checker.Check(context.Background(), steadyResolver())
	if err != nil || got.Status() != StatusUnknown {
		t.Fatalf("status = %s err = %v, want unknown", got.Status(), err)
	}
	if unavailableOf(got)[MetricCgroupMemory] != ReasonTargetChanged {
		t.Fatalf("unavailable = %v, want target_changed", got.Unavailable())
	}
}

func TestTargetChangeAcrossTheIntervalInvalidatesTargetMetricsOnly(t *testing.T) {
	changed := Target{Identity: "daemon-1:start-8", PID: 4242}
	cases := map[string]*sequenceResolver{
		"identity changed": {targets: []Target{testTarget, changed}},
		"pid changed":      {targets: []Target{testTarget, {Identity: testTarget.Identity, PID: 9}}},
		"lookup failed": {
			targets: []Target{testTarget, testTarget},
			errs:    []error{nil, errResolver},
		},
	}
	reasons := map[string]Reason{
		"identity changed": ReasonTargetChanged,
		"pid changed":      ReasonTargetChanged,
		"lookup failed":    ReasonTargetUnavailable,
	}
	for name, resolver := range cases {
		t.Run(name, func(t *testing.T) {
			psi := psiSeries(onePercentStall)
			samples := make([]reading[cgroupSample], 4)
			for i := range samples {
				samples[i] = cgroupAt(psi[i], memoryLevel(2*gib, 2*gib-100<<20))
			}
			ticks := withCgroup(withCPU(healthySnapshots(4), cpuSeries(0.99, 0.99, 0.99)), samples...)
			checker, _, _ := newTestChecker(t, ticks)
			got, err := checker.Check(context.Background(), resolver)
			if err != nil {
				t.Fatal(err)
			}
			if pressuresOf(got)[MetricHostCPU] != BasisSustained {
				t.Fatalf("pressures = %v, want host cpu to stand", got.Pressures())
			}
			if _, stillPressured := pressuresOf(got)[MetricCgroupMemory]; stillPressured {
				t.Fatalf("pressures = %v, want the stale cgroup pressure dropped", got.Pressures())
			}
			if unavailableOf(got)[MetricCgroupMemory] != reasons[name] {
				t.Fatalf("unavailable = %v, want %s", got.Unavailable(), reasons[name])
			}
		})
	}
}

func TestHiddenAncestorsStillRefuseOnVisiblePressure(t *testing.T) {
	samples := make([]reading[cgroupSample], 4)
	psi := psiSeries(onePercentStall)
	for i := range samples {
		sample := cgroupAt(psi[i], memoryLevel(2*gib, 2*gib-100<<20))
		sample.value.hiddenAncestors = true
		samples[i] = sample
	}
	got := checkMemory(t, withCgroup(healthySnapshots(4), samples...))
	if pressuresOf(got)[MetricCgroupMemory] != BasisSustained {
		t.Fatalf("pressures = %v, want visible pressure to refuse despite the coverage gap", got.Pressures())
	}
}
