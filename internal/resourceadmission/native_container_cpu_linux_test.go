//go:build linux

package resourceadmission

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// burners are cooperative busy loops. Each one stops on request, and join
// waits for every one, so no test path leaves a spinning goroutine behind.
type burners struct {
	stop    chan struct{}
	wg      sync.WaitGroup
	sink    atomic.Uint64
	started bool
	once    sync.Once
}

func newBurners() *burners { return &burners{stop: make(chan struct{})} }

// start launches one loop. One thread can use a 0.5 CPU quota up to its limit;
// several threads waste quota slices and reach it only about half of the time.
func (b *burners) start() {
	if b.started {
		return
	}
	b.started = true
	b.wg.Go(b.spin)
}

func (b *burners) spin() {
	var n uint64
	for {
		select {
		case <-b.stop:
			b.sink.Add(n)
			return
		default:
		}
		for i := range uint64(50_000) {
			n += i
		}
	}
}

func (b *burners) join() {
	b.once.Do(func() { close(b.stop) })
	b.wg.Wait()
}

// A cgroup quota is a cap, not a guarantee: on a busy host the container may
// receive less than 0.5 CPU and never saturate. Each scenario therefore makes
// a bounded number of attempts. Every attempt checks the verdict against its
// own real intervals in both directions, and a mismatch fails at once. When no
// attempt produced the scenario's precondition, the test fails with
// fixtureUnavailableMarker: the fixture was not demonstrated, which is not a
// verdict mismatch and not native proof.
const cpuAttempts = 5

// fixtureUnavailableMarker names a failed native test whose fixture never
// produced its precondition, so a log reader can tell it from a wrong verdict.
const fixtureUnavailableMarker = "NATIVE_FIXTURE_PRECONDITION_UNAVAILABLE"

func fixtureUnavailable(t *testing.T, fixture string, attempts int, want string) {
	t.Helper()
	t.Fatalf("%s fixture=%s attempts=%d: the host never delivered %s; every attempt's verdict matched its intervals",
		fixtureUnavailableMarker, fixture, attempts, want)
}

// cgroupCPUIntervals classifies each recorded interval with the production
// rule over the real cgroup usage counters and the elapsed time the checker
// itself measured.
func cgroupCPUIntervals(rec *tickRecorder) []outcome {
	var out []outcome
	for i, elapsed := range rec.intervals() {
		out = append(out, cgroupCPUOutcome(rec.snaps[i].cgroup, rec.snaps[i+1].cgroup, elapsed))
	}
	return out
}

// hotStreak is the longest run of consecutive saturated intervals. A clear or
// unknown interval ends a run, as it ends the checker's streak.
func hotStreak(outcomes []outcome) int {
	longest, run := 0, 0
	for _, o := range outcomes {
		if o.state != outcomeHot {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	return longest
}

// cpuInvalidated reports whether the checker withdrew cgroup CPU after
// sampling: its own deadline or a target revalidation. Such an attempt can
// neither confirm nor refute a streak.
func cpuInvalidated(got Assessment) bool {
	for _, reason := range []Reason{ReasonDeadline, ReasonTargetChanged, ReasonTargetUnavailable} {
		if containsUnavailable(got.Unavailable(), Unavailable{Metric: MetricCgroupCPU, Reason: reason}) {
			return true
		}
	}
	return false
}

// requireCPUVerdict checks one attempt in both directions: a cgroup CPU
// refusal needs requiredStreak consecutive saturated intervals, and such a
// streak must be refused unless the checker invalidated the metric. It
// returns whether the attempt was a qualified refusal.
func requireCPUVerdict(t *testing.T, attempt int, got Assessment, outcomes []outcome) bool {
	t.Helper()
	streak := hotStreak(outcomes)
	refused := containsPressure(got.Pressures(), Pressure{Metric: MetricCgroupCPU, Basis: BasisSustained})
	switch {
	case containsPressure(got.Pressures(), Pressure{Metric: MetricCgroupCPU, Basis: BasisCritical}):
		t.Fatalf("attempt %d: verdict mismatch: cgroup CPU has no critical rule but pressures %v", attempt, got.Pressures())
	case refused && streak < requiredStreak:
		t.Fatalf("attempt %d: verdict mismatch: refused with a longest streak of %d saturated intervals %v",
			attempt, streak, outcomes)
	case !refused && streak >= requiredStreak && !cpuInvalidated(got):
		t.Fatalf("attempt %d: verdict mismatch: %d saturated intervals in a row but pressures %v unavailable %v",
			attempt, streak, got.Pressures(), got.Unavailable())
	case refused && got.Status() != StatusPressure:
		t.Fatalf("attempt %d: refusal with status %s", attempt, got.Status())
	}
	return refused
}

// cgroupCPUUsage is the target level's cgroup CPU usage over one interval, or
// why it cannot be computed.
func cgroupCPUUsage(prev, cur reading[cgroupSample]) (uint64, string) {
	switch {
	case !prev.ok || !cur.ok:
		return 0, fmt.Sprintf("cgroup reading unavailable (%s, %s)", prev.failure(), cur.failure())
	case len(prev.value.levels) == 0 || len(cur.value.levels) == 0:
		return 0, "no cgroup level"
	}
	before, after := prev.value.levels[0].cpu, cur.value.levels[0].cpu
	switch {
	case !before.ok || !after.ok:
		return 0, fmt.Sprintf("cpu usage unavailable (%s, %s)", before.failure(), after.failure())
	case after.value.usageUS < before.value.usageUS:
		return 0, "cpu usage counter went backwards"
	}
	return after.value.usageUS - before.value.usageUS, ""
}

func logCPUIntervals(t *testing.T, attempt int, rec *tickRecorder) {
	t.Helper()
	outcomes := cgroupCPUIntervals(rec)
	for i, elapsed := range rec.intervals() {
		used, missing := cgroupCPUUsage(rec.snaps[i].cgroup, rec.snaps[i+1].cgroup)
		if missing != "" {
			t.Logf("attempt %d interval %d: %v elapsed, %s, outcome %+v", attempt, i, elapsed, missing, outcomes[i])
			continue
		}
		t.Logf("attempt %d interval %d: %v elapsed, cgroup usage %dus = %.3f of the 0.5 CPU quota, outcome %+v",
			attempt, i, elapsed, used, float64(used)/(float64(elapsed.Microseconds())*containerQuotaCPUs), outcomes[i])
	}
}

// checkUnderLoad runs one native Check whose load is scheduled by after.
func checkUnderLoad(t *testing.T, load *burners, after func(tick int)) (Assessment, *tickRecorder) {
	t.Helper()
	rec := &tickRecorder{observer: newNativeObserver(), after: after}
	ctx, cancel := context.WithTimeout(context.Background(), containerCheckWindow)
	defer cancel()
	started := time.Now()
	got, err := newChecker(rec, rec, checkBudget).Check(ctx, selfResolver{})
	elapsed := time.Since(started)
	load.join()
	if err != nil {
		t.Fatalf("Check error = %v", err)
	}
	if elapsed > checkBudget+time.Second {
		t.Fatalf("Check took %v, want it bounded by %v", elapsed, checkBudget)
	}
	return got, rec
}

func TestNativeContainerFiniteQuotaCPUSpikeIsObservedThenClears(t *testing.T) {
	requireOwnedContainer(t)
	containerSample(t)
	for attempt := 1; attempt <= cpuAttempts; attempt++ {
		load := newBurners()
		t.Cleanup(load.join)
		got, rec := checkUnderLoad(t, load, func(tick int) {
			switch tick {
			case 0:
				load.start()
			case 1:
				load.join()
			}
		})
		logCPUIntervals(t, attempt, rec)
		outcomes := cgroupCPUIntervals(rec)
		requireCPUVerdict(t, attempt, got, outcomes)
		if containsMetric(got.Pressures(), MetricCgroupCPU) {
			t.Fatalf("attempt %d: a finite spike of one interval was refused: %v", attempt, got.Pressures())
		}
		if len(outcomes) >= 2 && outcomes[0].state == outcomeHot && outcomes[1].state == outcomeClear {
			return
		}
	}
	fixtureUnavailable(t, "cgroup_cpu_spike", cpuAttempts, "a saturated interval followed by a cleared one")
}

func TestNativeContainerSustainedQuotaCPUSaturationIsRefused(t *testing.T) {
	requireOwnedContainer(t)
	containerSample(t)
	for attempt := 1; attempt <= cpuAttempts; attempt++ {
		load := newBurners()
		t.Cleanup(load.join)
		load.start()
		got, rec := checkUnderLoad(t, load, nil)
		logCPUIntervals(t, attempt, rec)
		if requireCPUVerdict(t, attempt, got, cgroupCPUIntervals(rec)) {
			return
		}
	}
	fixtureUnavailable(t, "cgroup_cpu_sustained", cpuAttempts, fmt.Sprintf("%d saturated intervals in a row", requiredStreak))
}

func containsMetric(all []Pressure, metric Metric) bool {
	for _, p := range all {
		if p.Metric == metric {
			return true
		}
	}
	return false
}

func containsPressure(all []Pressure, want Pressure) bool {
	for _, p := range all {
		if p == want {
			return true
		}
	}
	return false
}
