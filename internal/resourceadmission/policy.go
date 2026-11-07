package resourceadmission

import (
	"math"
	"time"
)

// Policy constants. They are locked by the approved resource-admission plan.
const (
	// sampleInterval is the spacing of sampling ticks.
	sampleInterval = time.Second
	// requiredStreak is the number of consecutive hot samples or intervals
	// that confirm sustained pressure.
	requiredStreak = 3
	// checkBudget is the sampling budget of one check. It bounds waits and
	// tick boundaries; it does not preempt a blocked operating-system read.
	checkBudget = 4 * time.Second
	// cpuBusyThreshold is the busy share that counts as CPU saturation.
	cpuBusyThreshold = 0.95
	// psiStallThreshold is the share of an interval the target must have
	// stalled on memory for cgroup memory pressure to count.
	psiStallThreshold = 0.01
	// reserveCeiling caps the memory reserve at 1 GiB.
	reserveCeiling uint64 = 1 << 30
	// reserveDivisor makes the reserve a tenth of the total.
	reserveDivisor uint64 = 10
)

// reserveBytes is the memory reserve for total bytes: min(1 GiB, 10%).
func reserveBytes(total uint64) uint64 {
	return min(reserveCeiling, total/reserveDivisor)
}

func hostMemoryOutcome(r reading[hostMemory]) outcome {
	if r.skipped() {
		return skippedOutcome
	}
	if !r.ok {
		return unknownOutcome(r.failure())
	}
	switch r.value.source {
	case memorySourceVMPressure:
		return classifyVMPressure(r.value.vm)
	case memorySourceAvailable:
		return classifyAvailable(r.value.total, r.value.available)
	}
	return unknownOutcome(ReasonMalformed)
}

// classifyAvailable judges Linux available memory against the host reserve:
// below half the reserve is critical, below the reserve is hot.
func classifyAvailable(total, available uint64) outcome {
	if total == 0 || available > total {
		return unknownOutcome(ReasonMalformed)
	}
	reserve := reserveBytes(total)
	switch {
	case available < reserve/2:
		return criticalOutcome
	case available < reserve:
		return hotOutcome
	default:
		return clearOutcome
	}
}

// classifyVMPressure judges the native macOS flag. Available memory is not a
// macOS refusal rule.
func classifyVMPressure(level vmPressure) outcome {
	switch level {
	case vmPressureNormal:
		return clearOutcome
	case vmPressureWarning:
		return hotOutcome
	case vmPressureCritical:
		return criticalOutcome
	}
	return unknownOutcome(ReasonMalformed)
}

func hostCPUOutcome(prev, cur reading[cpuCounters]) outcome {
	return evaluateHostCPU(prev, cur).outcome
}

type hostCPUEvaluation struct {
	outcome      outcome
	busyDelta    *float64
	totalDelta   *float64
	busyFraction *float64
}

// evaluateHostCPU derives the decision and its evidence together from the
// same sample pair. No consumer re-reads counters or re-evaluates pressure.
func evaluateHostCPU(prev, cur reading[cpuCounters]) hostCPUEvaluation {
	if cur.skipped() || prev.skipped() {
		return hostCPUEvaluation{outcome: skippedOutcome}
	}
	if !cur.ok {
		return hostCPUEvaluation{outcome: unknownOutcome(cur.failure())}
	}
	if !prev.ok {
		return hostCPUEvaluation{outcome: unknownOutcome(prev.failure())}
	}
	if !validCounters(prev.value) || !validCounters(cur.value) {
		return hostCPUEvaluation{outcome: unknownOutcome(ReasonMalformed)}
	}
	busy := cur.value.busy - prev.value.busy
	total := cur.value.total - prev.value.total
	result := hostCPUEvaluation{busyDelta: &busy, totalDelta: &total}
	switch {
	case busy < 0 || total < 0:
		result.outcome = unknownOutcome(ReasonCounterReset)
	case total == 0:
		result.outcome = unknownOutcome(ReasonNoElapsedTime)
	case busy > total:
		result.outcome = unknownOutcome(ReasonMalformed)
	default:
		fraction := busy / total
		result.busyFraction = &fraction
		result.outcome = boolOutcome(fraction >= cpuBusyThreshold)
	}
	return result
}

// validCounters rejects readings no kernel counter can produce.
func validCounters(c cpuCounters) bool {
	finite := !math.IsNaN(c.busy) && !math.IsInf(c.busy, 0) && !math.IsNaN(c.total) && !math.IsInf(c.total, 0)
	return finite && c.busy >= 0 && c.busy <= c.total
}

// cgroupPair validates that two cgroup readings describe the same target over
// a positive interval. When they do not, the returned outcome says why and
// the bool is false.
func cgroupPair(prev, cur reading[cgroupSample], elapsed time.Duration) (outcome, bool) {
	switch {
	case cur.skipped() || prev.skipped():
		return skippedOutcome, false
	case !cur.ok:
		return unknownOutcome(cur.failure()), false
	case !prev.ok:
		return unknownOutcome(prev.failure()), false
	case prev.value.path != cur.value.path || len(prev.value.levels) != len(cur.value.levels):
		return unknownOutcome(ReasonTargetChanged), false
	case elapsed.Microseconds() <= 0:
		return unknownOutcome(ReasonNoElapsedTime), false
	}
	return clearOutcome, true
}

func cgroupCPUOutcome(prev, cur reading[cgroupSample], elapsed time.Duration) outcome {
	if bad, ok := cgroupPair(prev, cur, elapsed); !ok {
		return bad
	}
	outcomes := make([]outcome, len(cur.value.levels))
	for i, level := range cur.value.levels {
		outcomes[i] = levelCPUOutcome(prev.value.levels[i].cpu, level.cpu, elapsed)
	}
	return withCoverage(anyOf(outcomes), cur.value)
}

// withCoverage keeps a gap visible when limits above the visible mount cannot
// be observed: a clear result becomes clear with scope_unresolved.
func withCoverage(o outcome, sample cgroupSample) outcome {
	if sample.hiddenAncestors && o.state == outcomeClear && o.reason == "" {
		o.reason = ReasonScopeUnresolved
	}
	return o
}

// cpuCapacity is the CPU count a level may use: the smaller of its quota and
// its cpuset. It is false for an unconstrained level.
func cpuCapacity(c levelCPU) (float64, bool) {
	switch {
	case c.hasQuota && c.cpusetCPUs > 0:
		return min(c.quotaCPUs, float64(c.cpusetCPUs)), true
	case c.hasQuota:
		return c.quotaCPUs, true
	case c.cpusetCPUs > 0:
		return float64(c.cpusetCPUs), true
	}
	return 0, false
}

// levelCPUOutcome pairs the level's own usage, which includes every sibling
// of the target below it, with the level's own capacity.
func levelCPUOutcome(prev, cur reading[levelCPU], elapsed time.Duration) outcome {
	if !cur.ok {
		return unknownOutcome(cur.failure())
	}
	capacity, constrained := cpuCapacity(cur.value)
	if !constrained {
		return clearOutcome
	}
	if capacity <= 0 {
		return unknownOutcome(ReasonMalformed)
	}
	if !prev.ok {
		return unknownOutcome(prev.failure())
	}
	// The usage delta can only be judged against one capacity. A changed
	// effective capacity leaves part of the interval measured against
	// another limit; a proportional period change or a cpuset change behind
	// an unchanged bottleneck yields the same capacity and is not a change.
	if prevCapacity, wasConstrained := cpuCapacity(prev.value); !wasConstrained || prevCapacity != capacity {
		return unknownOutcome(ReasonTargetChanged)
	}
	if cur.value.usageUS < prev.value.usageUS {
		return unknownOutcome(ReasonCounterReset)
	}
	used := float64(cur.value.usageUS - prev.value.usageUS)
	available := float64(elapsed.Microseconds()) * capacity
	return boolOutcome(used/available >= cpuBusyThreshold)
}

// cgroupMemoryOutcome is hot only when some constrained level has low
// estimated headroom AND the target's own memory PSI stalled for at least 1%
// of the interval. Ancestor or sibling PSI never counts. When any level is
// constrained or unreadable, missing PSI stays visible even if headroom is
// ample; when nothing is constrained PSI is irrelevant.
func cgroupMemoryOutcome(prev, cur reading[cgroupSample], elapsed time.Duration) outcome {
	if bad, ok := cgroupPair(prev, cur, elapsed); !ok {
		return bad
	}
	outcomes := make([]outcome, len(cur.value.levels))
	constrained := false
	for i, level := range cur.value.levels {
		outcomes[i] = levelMemoryOutcome(level.memory)
		constrained = constrained || !level.memory.ok || level.memory.value.max.set
	}
	if !constrained {
		return withCoverage(clearOutcome, cur.value)
	}
	psi := psiOutcome(prev.value.psiStallUS, cur.value.psiStallUS, elapsed)
	return withCoverage(allOf(anyOf(outcomes), psi), cur.value)
}

// levelMemoryOutcome is hot when the level has a hard limit and its estimated
// headroom is under the reserve for that limit.
func levelMemoryOutcome(r reading[levelMemory]) outcome {
	if !r.ok {
		return unknownOutcome(r.failure())
	}
	if !r.value.max.set {
		return clearOutcome
	}
	return boolOutcome(headroomLow(r.value.max.bytes, r.value.workingSet))
}

func headroomLow(limitBytes, workingSet uint64) bool {
	var headroom uint64
	if workingSet < limitBytes {
		headroom = limitBytes - workingSet
	}
	return headroom < reserveBytes(limitBytes)
}

func psiOutcome(prev, cur reading[uint64], elapsed time.Duration) outcome {
	if !cur.ok {
		return unknownOutcome(cur.failure())
	}
	if !prev.ok {
		return unknownOutcome(prev.failure())
	}
	if cur.value < prev.value {
		return unknownOutcome(ReasonCounterReset)
	}
	if elapsed.Microseconds() <= 0 {
		return unknownOutcome(ReasonNoElapsedTime)
	}
	stalled := float64(cur.value - prev.value)
	return boolOutcome(stalled/float64(elapsed.Microseconds()) >= psiStallThreshold)
}

// memoryHighNear reports whether any level's working-set estimate is within
// the reserve of memory.high. memory.high only throttles, so this is advisory.
func memoryHighNear(r reading[cgroupSample]) bool {
	if !r.ok {
		return false
	}
	for _, level := range r.value.levels {
		if level.memory.ok && level.memory.value.high.set &&
			headroomLow(level.memory.value.high.bytes, level.memory.value.workingSet) {
			return true
		}
	}
	return false
}
