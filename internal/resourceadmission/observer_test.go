package resourceadmission

import (
	"context"
	"testing"
)

func TestComposedObserverCombinesReaders(t *testing.T) {
	var gotTarget *Target
	obs := composedObserver{
		cpu:    func(context.Context) reading[cpuCounters] { return observed(cpuCounters{busy: 1, total: 2}) },
		memory: func(context.Context) reading[hostMemory] { return vmMemory(vmPressureNormal) },
		cgroup: func(target *Target) reading[cgroupSample] {
			gotTarget = target
			return notApplicable[cgroupSample]()
		},
		cgroupPath: func(target Target) reading[string] { return observed(target.CgroupPath) },
	}
	target := Target{Identity: "i", CgroupPath: "/x"}
	snap := obs.observe(context.Background(), &target)
	if !snap.cpu.ok || !snap.memory.ok || !snap.cgroup.skipped() {
		t.Fatalf("snapshot = %+v", snap)
	}
	if gotTarget != &target {
		t.Fatal("the target must reach the cgroup reader unchanged")
	}
	if got := obs.membership(context.Background(), target); got.value != "/x" {
		t.Fatalf("membership = %+v", got)
	}
}

func TestZeroReadingIsUnavailableNeverHealthy(t *testing.T) {
	var zero snapshot
	if hostMemoryOutcome(zero.memory).state != outcomeUnknown {
		t.Fatal("a zero memory reading must be unknown")
	}
	if hostCPUOutcome(zero.cpu, zero.cpu).state != outcomeUnknown {
		t.Fatal("a zero cpu reading must be unknown")
	}
}
