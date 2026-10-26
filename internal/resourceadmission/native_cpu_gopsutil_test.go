//go:build !linux

package resourceadmission

import (
	"context"
	"testing"

	"github.com/shirou/gopsutil/v4/cpu"
)

func TestCountersFromExcludesGuestIdleAndIowait(t *testing.T) {
	got := countersFrom(cpu.TimesStat{
		User: 10, System: 5, Idle: 70, Nice: 1, Iowait: 8, Irq: 1, Softirq: 2, Steal: 3,
		Guest: 4, GuestNice: 2,
	})
	want := cpuCounters{busy: 22, total: 100}
	if got != want {
		t.Fatalf("counters = %+v, want %+v", got, want)
	}
}

func TestReadHostCPUAdvances(t *testing.T) {
	first := readHostCPU(context.Background())
	second := readHostCPU(context.Background())
	if !first.ok || !second.ok {
		t.Skipf("host cpu unavailable on this platform: %s %s", first.failure(), second.failure())
	}
	if second.value.total < first.value.total || second.value.busy < first.value.busy {
		t.Fatalf("counters went backwards: %+v then %+v", first.value, second.value)
	}
}
