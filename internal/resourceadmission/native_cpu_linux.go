//go:build linux

package resourceadmission

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
)

// procStatFile is the fixed kernel file; no environment can redirect it.
const procStatFile = "/proc/stat"

// procStatLimit bounds the bytes read: the aggregate line is far smaller.
const procStatLimit = 4096

var errProcStat = errors.New("malformed /proc/stat aggregate cpu line")

// readHostCPU reads the aggregate cpu line of /proc/stat. It holds no state
// and takes no baseline; the checker pairs two readings. gopsutil's cpu
// package is not used on Linux because its import-time init reads the
// inherited HOST_PROC/stat before main.
func readHostCPU(context.Context) reading[cpuCounters] {
	f, err := os.Open(procStatFile)
	if err != nil {
		return unobserved[cpuCounters](ReasonUnreadable)
	}
	reader := readProcStatCPU(f)
	if closeErr := f.Close(); closeErr != nil {
		return unobserved[cpuCounters](ReasonUnreadable)
	}
	return reader
}

// readProcStatCPU reads at most procStatLimit bytes and parses the first line.
func readProcStatCPU(r io.Reader) reading[cpuCounters] {
	data, err := io.ReadAll(io.LimitReader(r, procStatLimit))
	if err != nil {
		return unobserved[cpuCounters](ReasonUnreadable)
	}
	line, _, found := bytes.Cut(data, []byte("\n"))
	if !found {
		return unobserved[cpuCounters](ReasonMalformed)
	}
	counters, err := parseProcStatCPU(string(line))
	if err != nil {
		return unobserved[cpuCounters](ReasonMalformed)
	}
	return observed(counters)
}

// parseProcStatCPU parses "cpu user nice system idle iowait irq softirq
// steal ...". Guest and guest_nice are already counted in user and nice, so
// they are left out. Jiffies are not converted: busy/total ratios do not
// depend on the tick rate.
func parseProcStatCPU(line string) (cpuCounters, error) {
	fields := bytes.Fields([]byte(line))
	if len(fields) < 5 || string(fields[0]) != "cpu" {
		return cpuCounters{}, errProcStat
	}
	const exclusive = 8 // user nice system idle iowait irq softirq steal
	var values [exclusive]uint64
	for i := 0; i < exclusive && i+1 < len(fields); i++ {
		v, err := strconv.ParseUint(string(fields[i+1]), 10, 64)
		if err != nil {
			return cpuCounters{}, fmt.Errorf("%w: %w", errProcStat, err)
		}
		values[i] = v
	}
	if len(fields)-1 > exclusive {
		for _, extra := range fields[exclusive+1:] {
			if _, err := strconv.ParseUint(string(extra), 10, 64); err != nil {
				return cpuCounters{}, fmt.Errorf("%w: %w", errProcStat, err)
			}
		}
	}
	var total uint64
	for _, v := range values {
		if total > math.MaxUint64-v {
			return cpuCounters{}, fmt.Errorf("%w: overflow", errProcStat)
		}
		total += v
	}
	idle := values[3]
	iowait := values[4]
	if total == 0 {
		return cpuCounters{}, fmt.Errorf("%w: zero total", errProcStat)
	}
	return cpuCounters{busy: float64(total - idle - iowait), total: float64(total)}, nil
}
