// Package resourceadmission observes host and execution-target resource
// pressure for one admission check and classifies it.
//
// Every Check is invocation-local: it keeps no history, writes no global
// state, starts no process and sends no signal. Pressure is reported only
// when a metric stays under pressure across consecutive sampled intervals; a
// missing or unreliable metric is reported as unavailable and never as zero.
package resourceadmission

import (
	"slices"
	"time"
)

// Status is the overall outcome of one check.
type Status string

// The three statuses mirror the public resource_admission metadata values.
const (
	StatusClear    Status = "clear"
	StatusPressure Status = "pressure"
	StatusUnknown  Status = "unknown"
)

// Metric names one monitored dimension.
type Metric string

// The monitored dimensions. Memory on macOS is the native VM pressure level.
const (
	MetricHostCPU      Metric = "host_cpu"
	MetricHostMemory   Metric = "host_memory"
	MetricCgroupCPU    Metric = "cgroup_cpu"
	MetricCgroupMemory Metric = "cgroup_memory"
)

// Basis states how a pressure was confirmed.
type Basis string

// BasisSustained needs consecutive low samples; BasisCritical is immediate.
const (
	BasisSustained Basis = "sustained"
	BasisCritical  Basis = "critical"
)

// Reason is a stable, secret-free code for why a metric is unavailable.
type Reason string

// Unavailability reasons. They carry no paths, PIDs or raw error text.
const (
	ReasonTargetUnavailable   Reason = "target_unavailable"
	ReasonTargetChanged       Reason = "target_changed"
	ReasonScopeUnresolved     Reason = "scope_unresolved"
	ReasonCgroupUnsupported   Reason = "cgroup_unsupported"
	ReasonPressureUnavailable Reason = "pressure_unavailable"
	ReasonUnreadable          Reason = "unreadable"
	ReasonMalformed           Reason = "malformed"
	ReasonCounterReset        Reason = "counter_reset"
	ReasonNoElapsedTime       Reason = "no_elapsed_time"
	ReasonDeadline            Reason = "deadline"
	ReasonUnsupportedPlatform Reason = "unsupported_platform"
)

// Advisory is a non-gating observation that never changes the status.
type Advisory string

// AdvisoryMemoryHighNear reports that a cgroup memory.high throttle point is
// near. memory.high throttles; it does not kill, so it never refuses.
const AdvisoryMemoryHighNear Advisory = "memory_high_headroom_low"

// Pressure is one confirmed pressure.
type Pressure struct {
	Metric Metric
	Basis  Basis
}

// Unavailable is one metric that could not be judged.
type Unavailable struct {
	Metric Metric
	Reason Reason
}

// Findings are the per-metric results folded into an Assessment.
type Findings struct {
	Pressures   []Pressure
	Unavailable []Unavailable
	Advisories  []Advisory
	HostCPU     *HostCPUDiagnostics
}

// Scope identifies what an assessment was measured against. Two assessments
// of the same Scope may be compared; the zero Scope means the target was not
// resolved.
type Scope struct {
	TargetIdentity string
	Cgroup         string
}

// freshnessWindow is how long an assessment may be reused for the same Scope.
const freshnessWindow = 5 * time.Second

// Assessment is the immutable result of one check. It is invocation-local and
// never persisted.
type Assessment struct {
	built      bool
	observedAt time.Time
	scope      Scope
	findings   Findings
}

// NewAssessment builds an Assessment, copying the findings.
func NewAssessment(observedAt time.Time, scope Scope, findings Findings) Assessment {
	return Assessment{
		built:      true,
		observedAt: observedAt,
		scope:      scope,
		findings: Findings{
			Pressures:   slices.Clone(findings.Pressures),
			Unavailable: slices.Clone(findings.Unavailable),
			Advisories:  slices.Clone(findings.Advisories),
			HostCPU:     copyHostCPUDiagnostics(findings.HostCPU),
		},
	}
}

// Status is pressure when any pressure is confirmed, otherwise unknown when
// any metric is unavailable, otherwise clear. Confirmed pressure wins over a
// missing metric. The zero Assessment was never observed and is unknown.
func (a Assessment) Status() Status {
	switch {
	case !a.built:
		return StatusUnknown
	case len(a.findings.Pressures) > 0:
		return StatusPressure
	case len(a.findings.Unavailable) > 0:
		return StatusUnknown
	default:
		return StatusClear
	}
}

// Pressures returns a copy of the confirmed pressures.
func (a Assessment) Pressures() []Pressure { return slices.Clone(a.findings.Pressures) }

// Unavailable returns a copy of the metrics that could not be judged.
func (a Assessment) Unavailable() []Unavailable { return slices.Clone(a.findings.Unavailable) }

// Advisories returns a copy of the non-gating observations.
func (a Assessment) Advisories() []Advisory { return slices.Clone(a.findings.Advisories) }

// HostCPUDiagnostics returns a defensive copy only for confirmed host CPU
// pressure. Other assessments keep their public representation unchanged.
func (a Assessment) HostCPUDiagnostics() *HostCPUDiagnostics {
	for _, pressure := range a.findings.Pressures {
		if pressure.Metric == MetricHostCPU {
			return copyHostCPUDiagnostics(a.findings.HostCPU)
		}
	}
	return nil
}

// ObservedAt is the clock reading when the check finished.
func (a Assessment) ObservedAt() time.Time { return a.observedAt }

// Scope is what the assessment was measured against.
func (a Assessment) Scope() Scope { return a.scope }

// Reusable reports whether the assessment still covers current: it was
// observed no more than five seconds before now and for the same Scope. now
// and ObservedAt should both carry monotonic readings.
func (a Assessment) Reusable(now time.Time, current Scope) bool {
	age := now.Sub(a.observedAt)
	return a.built && age >= 0 && age <= freshnessWindow && a.scope == current
}
