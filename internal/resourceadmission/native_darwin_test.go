//go:build darwin

package resourceadmission

import (
	"context"
	"testing"
	"time"
)

func TestNativeDarwinMemoryIsTheVMPressureFlag(t *testing.T) {
	got := readHostMemory(context.Background())
	if !got.ok {
		t.Fatalf("memory unavailable: %s", got.failure())
	}
	if out := classifyVMPressure(got.value.vm); out.state == outcomeUnknown {
		t.Fatalf("kernel returned vm pressure %d, want 1, 2 or 4", got.value.vm)
	}
}

func TestNativeDarwinHasNoCgroupMetrics(t *testing.T) {
	snap := newNativeObserver().observe(context.Background(), &Target{Identity: "i", PID: 1})
	if !snap.cgroup.skipped() {
		t.Fatalf("cgroup = %+v, want not applicable", snap.cgroup)
	}
}

// darwinCheckWindow bounds one native Check: its own four-second sampling
// budget plus slack for a slow kernel read, so a stuck read fails the test.
const darwinCheckWindow = checkBudget + 2*time.Second

type countingResolver struct{ calls int }

func (r *countingResolver) ResolveTarget(context.Context) (Target, error) {
	r.calls++
	return Target{Identity: "unused", PID: 1}, nil
}

func TestNativeDarwinCPUCountersAdvanceAcrossAPositiveInterval(t *testing.T) {
	first := readHostCPU(context.Background())
	time.Sleep(sampleInterval)
	second := readHostCPU(context.Background())
	if !first.ok || !second.ok {
		t.Fatalf("host cpu unreadable: %s then %s", first.failure(), second.failure())
	}
	if second.value.total <= first.value.total || second.value.busy < first.value.busy {
		t.Fatalf("counters did not advance over %v: %+v then %+v", sampleInterval, first.value, second.value)
	}
	if first.value.busy > first.value.total || second.value.busy > second.value.total {
		t.Fatalf("busy exceeds total: %+v then %+v", first.value, second.value)
	}
	out := hostCPUOutcome(first, second)
	if out.state != outcomeClear && out.state != outcomeHot {
		t.Fatalf("a real positive interval classified as %+v", out)
	}
	t.Logf("real %v interval: busy %0.3f of total %0.3f CPU seconds, outcome %d",
		sampleInterval, second.value.busy-first.value.busy, second.value.total-first.value.total, out.state)
}

func TestNativeDarwinCheckSamplesRealKernelReadsAndNeverResolvesATarget(t *testing.T) {
	rec := &tickRecorder{observer: newNativeObserver()}
	resolver := &countingResolver{}
	ctx, cancel := context.WithTimeout(context.Background(), darwinCheckWindow)
	defer cancel()
	started := time.Now()
	got, err := newChecker(rec, rec, checkBudget).Check(ctx, resolver)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Check error = %v", err)
	}
	if elapsed > darwinCheckWindow {
		t.Fatalf("Check took %v, want it bounded by %v", elapsed, darwinCheckWindow)
	}
	if resolver.calls != 0 || got.Scope() != (Scope{}) {
		t.Fatalf("resolver calls %d scope %+v, want no target on a platform without cgroups", resolver.calls, got.Scope())
	}
	requireHostMetricsOnly(t, got)
	if got.ObservedAt().IsZero() {
		t.Fatal("assessment carries no observation time")
	}
	requireDarwinTicks(t, rec, got)
	t.Logf("status %s pressures %v unavailable %v in %v over %d ticks", got.Status(), got.Pressures(), got.Unavailable(), elapsed, len(rec.snaps))
}

// requireHostMetricsOnly checks that every finding is a host metric and that
// the status matches the findings, without asserting the machine is healthy.
func requireHostMetricsOnly(t *testing.T, got Assessment) {
	t.Helper()
	for _, p := range got.Pressures() {
		if p.Metric != MetricHostCPU && p.Metric != MetricHostMemory {
			t.Fatalf("pressure on %s, want host metrics only on macOS", p.Metric)
		}
	}
	for _, u := range got.Unavailable() {
		if u.Metric != MetricHostCPU && u.Metric != MetricHostMemory {
			t.Fatalf("warning on %s, want host metrics only on macOS", u.Metric)
		}
	}
	wantStatus := StatusClear
	switch {
	case len(got.Pressures()) > 0:
		wantStatus = StatusPressure
	case len(got.Unavailable()) > 0:
		wantStatus = StatusUnknown
	}
	if got.Status() != wantStatus {
		t.Fatalf("status %s disagrees with pressures %v and warnings %v", got.Status(), got.Pressures(), got.Unavailable())
	}
}

// requireDarwinTicks checks the real counters behind the result. A critical
// kernel flag legitimately ends the check before any interval is sampled.
func requireDarwinTicks(t *testing.T, rec *tickRecorder, got Assessment) {
	t.Helper()
	if len(rec.snaps) == 0 {
		if len(got.Pressures()) != 1 || got.Pressures()[0] != (Pressure{Metric: MetricHostMemory, Basis: BasisCritical}) {
			t.Fatalf("no tick was sampled but the result is %v", got.Pressures())
		}
		return
	}
	if len(rec.snaps) < 2 {
		t.Fatalf("sampled %d tick, want a baseline and at least one interval", len(rec.snaps))
	}
	for i, snap := range rec.snaps {
		requireKernelTick(t, i, snap)
		if i > 0 && snap.cpu.value.total <= rec.snaps[i-1].cpu.value.total {
			t.Fatalf("tick %d: cpu total %v did not advance past %v", i, snap.cpu.value.total, rec.snaps[i-1].cpu.value.total)
		}
	}
	for i, d := range rec.intervals() {
		if d < sampleInterval/2 {
			t.Fatalf("interval %d lasted %v, want about %v", i, d, sampleInterval)
		}
	}
}

// requireKernelTick checks one tick read the real CPU counters and the real
// kernel VM pressure flag, and no cgroup metric.
func requireKernelTick(t *testing.T, tick int, snap snapshot) {
	t.Helper()
	if !snap.cpu.ok || !snap.memory.ok || !snap.cgroup.skipped() {
		t.Fatalf("tick %d: cpu %v memory %v cgroup %+v", tick, snap.cpu.failure(), snap.memory.failure(), snap.cgroup)
	}
	if level := snap.memory.value.vm; level != vmPressureNormal && level != vmPressureWarning && level != vmPressureCritical {
		t.Fatalf("tick %d: kernel vm pressure flag %d, want 1, 2 or 4", tick, level)
	}
}
