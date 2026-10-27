//go:build !linux

package resourceadmission

import (
	"context"

	"github.com/shirou/gopsutil/v4/cpu"
)

// readHostCPU reads the cumulative host CPU times. It never computes a
// percentage: gopsutil's Percent keeps package-global history. Linux does not
// use gopsutil's cpu package because its import-time init reads the inherited
// HOST_PROC/stat before main.
func readHostCPU(ctx context.Context) reading[cpuCounters] {
	times, err := cpu.TimesWithContext(ctx, false)
	if err != nil || len(times) != 1 {
		return unobserved[cpuCounters](ReasonUnreadable)
	}
	return observed(countersFrom(times[0]))
}

// countersFrom sums the exclusive CPU states. Guest time is already counted
// in user time on Linux, so it is left out; idle and iowait are not busy.
func countersFrom(t cpu.TimesStat) cpuCounters {
	total := t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal
	return cpuCounters{busy: total - t.Idle - t.Iowait, total: total}
}
