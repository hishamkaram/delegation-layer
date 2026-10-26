//go:build linux

package resourceadmission

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

var errMalformed = errors.New("malformed cgroup value")

const deletedSuffix = " (deleted)"

// placement is what /proc/<pid>/cgroup says beyond the v2 path, which the
// canonical lifetime resolver owns. v1 lines are only inspected to learn
// which controllers live outside the v2 hierarchy; v1 is not interpreted.
type placement struct {
	// deleted is set when the v2 cgroup has been removed.
	deleted bool
	// memoryOutside and cpuOutside are set when a v1 hierarchy carries the
	// memory or the cpu/cpuset controller, so the v2 files cannot describe it.
	memoryOutside bool
	cpuOutside    bool
}

// parsePlacement reads /proc/<pid>/cgroup. A system without a "0::" line has
// no unified hierarchy for this process.
func parsePlacement(data string) (placement, Reason) {
	var p placement
	found := false
	for line := range strings.SplitSeq(data, "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		if fields[0] == "0" && fields[1] == "" {
			found = true
			p.deleted = strings.HasSuffix(fields[2], deletedSuffix)
			continue
		}
		for controller := range strings.SplitSeq(fields[1], ",") {
			p.memoryOutside = p.memoryOutside || controller == "memory"
			p.cpuOutside = p.cpuOutside || controller == "cpu" || controller == "cpuacct" || controller == "cpuset"
		}
	}
	if !found {
		return placement{}, ReasonCgroupUnsupported
	}
	return p, ""
}

// chain lists dir and each of its parents up to and including top, at most
// limit levels. complete is false when levels remain beyond the limit; the
// list is never built past it.
func chain(dir, top string, limit int) (dirs []string, complete bool) {
	for {
		dirs = append(dirs, dir)
		if dir == top || dir == "." {
			return dirs, true
		}
		if len(dirs) == limit {
			return dirs, false
		}
		dir = path.Dir(dir)
	}
}

func parseUint(data string) (uint64, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(data), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errMalformed, err)
	}
	return value, nil
}

// parseLimit parses memory.max and memory.high: "max" means no limit.
func parseLimit(data string) (limit, error) {
	if strings.TrimSpace(data) == "max" {
		return limit{}, nil
	}
	value, err := parseUint(data)
	if err != nil {
		return limit{}, err
	}
	return limit{bytes: value, set: true}, nil
}

// keyedValue returns the value of key in a flat "key value" file such as
// memory.stat or cpu.stat.
func keyedValue(data, key string) (uint64, error) {
	for line := range strings.SplitSeq(data, "\n") {
		name, value, found := strings.Cut(line, " ")
		if found && name == key {
			return parseUint(value)
		}
	}
	return 0, fmt.Errorf("%w: missing %s", errMalformed, key)
}

// parseCPUMax parses cpu.max into a CPU count. A "max" quota is no quota.
func parseCPUMax(data string) (cpus float64, hasQuota bool, err error) {
	fields := strings.Fields(data)
	if len(fields) != 2 {
		return 0, false, fmt.Errorf("%w: cpu.max", errMalformed)
	}
	period, err := parseUint(fields[1])
	if err != nil || period == 0 {
		return 0, false, fmt.Errorf("%w: cpu.max period", errMalformed)
	}
	if fields[0] == "max" {
		return 0, false, nil
	}
	quota, err := parseUint(fields[0])
	if err != nil {
		return 0, false, err
	}
	return float64(quota) / float64(period), true, nil
}

// parseCPUSet counts the CPUs in a list such as "0-3,8,10-11".
func parseCPUSet(data string) (int, error) {
	list := strings.TrimSpace(data)
	if list == "" {
		return 0, fmt.Errorf("%w: empty cpuset", errMalformed)
	}
	count := 0
	for part := range strings.SplitSeq(list, ",") {
		first, last, isRange := strings.Cut(part, "-")
		low, err := strconv.Atoi(first)
		if err != nil || low < 0 {
			return 0, fmt.Errorf("%w: cpuset %q", errMalformed, part)
		}
		high := low
		if isRange {
			if high, err = strconv.Atoi(last); err != nil || high < low {
				return 0, fmt.Errorf("%w: cpuset %q", errMalformed, part)
			}
		}
		count += high - low + 1
	}
	return count, nil
}

// parsePSISomeTotal returns the cumulative "some" stall time in microseconds
// from a memory.pressure file.
func parsePSISomeTotal(data string) (uint64, error) {
	for line := range strings.SplitSeq(data, "\n") {
		rest, found := strings.CutPrefix(line, "some ")
		if !found {
			continue
		}
		for field := range strings.FieldsSeq(rest) {
			if total, isTotal := strings.CutPrefix(field, "total="); isTotal {
				return parseUint(total)
			}
		}
	}
	return 0, fmt.Errorf("%w: psi some total", errMalformed)
}
