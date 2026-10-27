//go:build linux

package resourceadmission

import (
	"context"
	"os"
	"testing"
	"time"
)

// The container tests read the real kernel inside one owned container run
// with --memory 128m --memory-swap 128m --cpus 0.5. They are opt-in: a run
// outside that container leaves the variable unset and skips. A run that sets
// it must find exactly that container, so a missing mapping fails loudly.
const (
	nativeContainerEnv     = "DELEGATE_NATIVE_RESOURCE_CONTAINER"
	nativeContainerProfile = "memory128MiB-cpu0.5"
	containerMemoryBytes   = uint64(128) << 20
	containerQuotaCPUs     = 0.5
	// containerCheckWindow is the caller deadline of each native Check: the
	// check's own budget plus slack, so a stuck read fails instead of hanging.
	containerCheckWindow = checkBudget + 6*time.Second
)

var containerTarget = Target{Identity: "native-container", PID: os.Getpid()}

func requireOwnedContainer(t *testing.T) {
	t.Helper()
	switch got := os.Getenv(nativeContainerEnv); got {
	case "":
		t.Skipf("set %s=%s inside the owned 128MiB/0.5CPU container to run this test", nativeContainerEnv, nativeContainerProfile)
	case nativeContainerProfile:
	default:
		t.Fatalf("%s=%q, want %q", nativeContainerEnv, got, nativeContainerProfile)
	}
}

// containerSample reads the real cgroup of this process and requires the
// exact 128MiB limit and 0.5 CPU quota, at the target and from the host view.
func containerSample(t *testing.T) cgroupSample {
	t.Helper()
	snap := newNativeObserver().observe(context.Background(), &containerTarget)
	if !snap.cgroup.ok {
		t.Fatalf("no cgroup reading in the owned container: %s", snap.cgroup.failure())
	}
	sample := snap.cgroup.value
	if sample.hiddenAncestors {
		t.Fatalf("cgroup %q has hidden ancestors; the container must expose its own cgroup root", sample.path)
	}
	var memoryFound, cpuFound bool
	for _, level := range sample.levels {
		if level.memory.ok && level.memory.value.max.set && level.memory.value.max.bytes == containerMemoryBytes {
			memoryFound = true
		}
		if level.cpu.ok && level.cpu.value.hasQuota && level.cpu.value.quotaCPUs == containerQuotaCPUs {
			cpuFound = true
		}
	}
	if !memoryFound || !cpuFound {
		t.Fatalf("levels %+v: memory.max 128MiB found %t, cpu quota 0.5 found %t", sample.levels, memoryFound, cpuFound)
	}
	return sample
}

func TestNativeContainerReadsTheEffectiveLimitsNotTheHostMetrics(t *testing.T) {
	requireOwnedContainer(t)
	sample := containerSample(t)
	host := readHostMemory(context.Background())
	if !host.ok {
		t.Fatalf("host memory unreadable: %s", host.failure())
	}
	if host.value.total <= containerMemoryBytes {
		t.Fatalf("host memory total %d is not larger than the %d byte container limit", host.value.total, containerMemoryBytes)
	}
	t.Logf("cgroup %s: memory.max %d, host MemTotal %d, levels %d", sample.path, containerMemoryBytes, host.value.total, len(sample.levels))
	for i, level := range sample.levels {
		t.Logf("level %d %s: memory %+v cpu %+v", i, level.path, level.memory.value, level.cpu.value)
	}
}

func TestNativeContainerIdleCheckReportsOnlyHostPressureOutsideTheCgroup(t *testing.T) {
	requireOwnedContainer(t)
	sample := containerSample(t)
	ctx, cancel := context.WithTimeout(context.Background(), containerCheckWindow)
	defer cancel()
	got, err := NewChecker().Check(ctx, selfResolver{})
	if err != nil {
		t.Fatalf("Check error = %v", err)
	}
	for _, p := range got.Pressures() {
		if p.Metric == MetricHostMemory && p.Basis == BasisCritical {
			t.Skipf("host memory is critical, so the check ended before the cgroup metrics (valid): %v", got.Pressures())
		}
	}
	if got.Scope().Cgroup != sample.path {
		t.Fatalf("scope cgroup %q, want %q", got.Scope().Cgroup, sample.path)
	}
	for _, p := range got.Pressures() {
		if p.Metric == MetricCgroupCPU || p.Metric == MetricCgroupMemory {
			t.Fatalf("an idle process in the container reports %+v", p)
		}
	}
	requireOnlyMissingPSIWarning(t, got)
	t.Logf("idle: status %s pressures %v unavailable %v advisories %v", got.Status(), got.Pressures(), got.Unavailable(), got.Advisories())
}

// requireOnlyMissingPSIWarning allows exactly one cgroup warning: memory
// pressure stall information the kernel does not provide.
func requireOnlyMissingPSIWarning(t *testing.T, got Assessment) {
	t.Helper()
	for _, u := range got.Unavailable() {
		switch {
		case u.Metric == MetricCgroupMemory && u.Reason == ReasonPressureUnavailable:
		case u.Metric == MetricCgroupCPU || u.Metric == MetricCgroupMemory:
			t.Fatalf("cgroup metric unavailable in the opted-in container: %+v", u)
		}
	}
}
