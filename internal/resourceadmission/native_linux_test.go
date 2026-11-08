//go:build linux

package resourceadmission

import (
	"context"
	"os"
	"testing"
	"time"
)

type selfResolver struct{}

func (selfResolver) ResolveTarget(context.Context) (Target, error) {
	return Target{Identity: "test-self", PID: os.Getpid()}, nil
}

func TestNativeLinuxReadersReturnRealValues(t *testing.T) {
	memory := readHostMemory(context.Background())
	if !memory.ok || memory.value.total == 0 || memory.value.available > memory.value.total {
		t.Fatalf("memory = %+v, want a real /proc/meminfo reading", memory)
	}
	cpuReading := readHostCPU(context.Background())
	if !cpuReading.ok || cpuReading.value.total <= 0 {
		t.Fatalf("cpu = %+v", cpuReading)
	}
}

func TestNativeLinuxCheckRespectsTheBudgetAndIdentifiesPressure(t *testing.T) {
	started := time.Now()
	got, err := NewChecker(GuardPolicy{}).Check(context.Background(), selfResolver{})
	if err != nil {
		t.Fatalf("Check error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > checkBudget+time.Second {
		t.Fatalf("Check took %v, want it bounded by %v", elapsed, checkBudget)
	}
	seen := map[Metric]bool{}
	for _, p := range got.Pressures() {
		seen[p.Metric] = true
	}
	for _, u := range got.Unavailable() {
		seen[u.Metric] = true
	}
	if got.Status() == StatusPressure && len(seen) == 0 {
		t.Fatal("pressure without a metric")
	}
}

func TestNativeLinuxScopeMatchesProcSelfCgroup(t *testing.T) {
	got, err := NewChecker(GuardPolicy{}).Scope(context.Background(), selfResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Cgroup == "" {
		t.Skip("no resolvable cgroup v2 membership on this host")
	}
	if _, err := os.Stat(got.Cgroup); err != nil {
		t.Fatalf("scope cgroup %q is not a real directory: %v", got.Cgroup, err)
	}
}

func TestNativeLinuxSampleMapsTheTargetThroughTheDiscoveredMount(t *testing.T) {
	snap := newNativeObserver().observe(context.Background(), &Target{Identity: "test-self", PID: os.Getpid()})
	if !snap.cgroup.ok {
		t.Skipf("no resolvable cgroup v2 membership on this host: %s", snap.cgroup.failure())
	}
	sample := snap.cgroup.value
	if len(sample.levels) == 0 || "/"+sample.levels[0].path != sample.path {
		t.Fatalf("path %q levels %v, want the first level to be the target directory", sample.path, sample.levels)
	}
	if _, err := os.Stat(sample.path); err != nil {
		t.Fatalf("target cgroup %q is not a directory on this host: %v", sample.path, err)
	}
	if !snap.cpu.ok || !snap.memory.ok {
		t.Fatalf("host readings unavailable: cpu %v memory %v", snap.cpu.failure(), snap.memory.failure())
	}
}
