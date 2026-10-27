package resourceadmission

// evaluator folds the snapshots of one check into per-metric sequences. It
// lives only for that check.
type evaluator struct {
	hostCPU       track
	hostMemory    track
	cgroupCPU     track
	cgroupMemory  track
	memoryHighHit bool
	cgroupPath    string
}

// step folds one snapshot. prev is nil for the baseline. Memory is judged on
// each sample; CPU and PSI rates are judged over the interval since prev.
func (e *evaluator) step(prev *snapshot, cur snapshot) {
	if prev == nil {
		e.expect(cur)
		e.hostMemory.baseline(hostMemoryOutcome(cur.memory))
	} else {
		e.hostMemory.record(hostMemoryOutcome(cur.memory))
	}
	if cur.cgroup.ok {
		if e.cgroupPath == "" {
			e.cgroupPath = cur.cgroup.value.path
		}
		e.memoryHighHit = e.memoryHighHit || memoryHighNear(cur.cgroup)
	}
	if prev == nil {
		return
	}
	elapsed := cur.at.Sub(prev.at)
	e.hostCPU.record(hostCPUOutcome(prev.cpu, cur.cpu))
	e.cgroupCPU.record(cgroupCPUOutcome(prev.cgroup, cur.cgroup, elapsed))
	e.cgroupMemory.record(cgroupMemoryOutcome(prev.cgroup, cur.cgroup, elapsed))
}

// expect registers every metric the platform provides so that a check cut
// short reports it unavailable instead of omitting it.
func (e *evaluator) expect(first snapshot) {
	e.hostCPU.seen = !first.cpu.skipped()
	e.hostMemory.seen = !first.memory.skipped()
	e.cgroupCPU.seen = !first.cgroup.skipped()
	e.cgroupMemory.seen = !first.cgroup.skipped()
}

// pending reports whether any metric is hot but not yet confirmed.
func (e *evaluator) pending() bool {
	return e.hostCPU.pending() || e.hostMemory.pending() ||
		e.cgroupCPU.pending() || e.cgroupMemory.pending()
}

func (e *evaluator) confirmed() bool {
	return e.hostCPU.confirmed() || e.hostMemory.confirmed() ||
		e.cgroupCPU.confirmed() || e.cgroupMemory.confirmed()
}

// expire marks every unfinished metric unavailable.
func (e *evaluator) expire(reason Reason) {
	for _, m := range e.metrics() {
		m.track.expire(reason)
	}
}

// invalidateTarget drops the target-specific metrics, including a confirmed
// pressure, because the target they were measured on can no longer be vouched
// for.
func (e *evaluator) invalidateTarget(reason Reason) {
	e.cgroupCPU.invalidate(reason)
	e.cgroupMemory.invalidate(reason)
	e.memoryHighHit = false
}

type metricTrack struct {
	metric Metric
	track  *track
}

func (e *evaluator) metrics() []metricTrack {
	return []metricTrack{
		{MetricHostCPU, &e.hostCPU},
		{MetricHostMemory, &e.hostMemory},
		{MetricCgroupCPU, &e.cgroupCPU},
		{MetricCgroupMemory, &e.cgroupMemory},
	}
}

// findings reports confirmed pressures, then unavailable metrics. A metric
// with a confirmed pressure is not also reported unavailable.
func (e *evaluator) findings() Findings {
	var f Findings
	for _, m := range e.metrics() {
		switch {
		case m.track.critical:
			f.Pressures = append(f.Pressures, Pressure{Metric: m.metric, Basis: BasisCritical})
		case m.track.sustained:
			f.Pressures = append(f.Pressures, Pressure{Metric: m.metric, Basis: BasisSustained})
		case m.track.seen && m.track.unknown != "":
			f.Unavailable = append(f.Unavailable, Unavailable{Metric: m.metric, Reason: m.track.unknown})
		}
	}
	if e.memoryHighHit {
		f.Advisories = append(f.Advisories, AdvisoryMemoryHighNear)
	}
	return f
}
