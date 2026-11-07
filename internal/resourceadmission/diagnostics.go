package resourceadmission

import (
	"math"
	"slices"
	"time"
)

// CPUCounterUnit names the native unit of the consumed host CPU counters.
type CPUCounterUnit string

const (
	CPUCounterTicks   CPUCounterUnit = "ticks"
	CPUCounterSeconds CPUCounterUnit = "seconds"
)

// CPUIntervalClassification is the evaluator's verdict for one interval.
type CPUIntervalClassification string

const (
	CPUIntervalClear       CPUIntervalClassification = "clear"
	CPUIntervalHot         CPUIntervalClassification = "hot"
	CPUIntervalUnavailable CPUIntervalClassification = "unavailable"
)

// HostCPUInterval contains only bounded, secret-free evidence from a consumed
// sample pair. Missing numbers are absent, never invented zero readings.
type HostCPUInterval struct {
	ElapsedMilliseconds int64
	BusyDelta           *float64
	TotalDelta          *float64
	BusyFraction        *float64
	Classification      CPUIntervalClassification
	UnavailableReason   Reason
}

// HostCPUDiagnostics describes the host CPU evidence of one check. The
// invocation's evaluator is its only production writer; Assessment owns an
// immutable copy and presentation consumers receive defensive copies.
type HostCPUDiagnostics struct {
	CounterUnit    CPUCounterUnit
	BusyThreshold  float64
	RequiredStreak int
	Intervals      []HostCPUInterval
}

func hostCPUInterval(elapsed time.Duration, result hostCPUEvaluation) HostCPUInterval {
	interval := HostCPUInterval{
		ElapsedMilliseconds: elapsed.Milliseconds(),
		BusyDelta:           result.busyDelta, TotalDelta: result.totalDelta, BusyFraction: result.busyFraction,
		Classification: CPUIntervalUnavailable, UnavailableReason: result.outcome.reason,
	}
	switch result.outcome.state {
	case outcomeHot:
		interval.Classification = CPUIntervalHot
	case outcomeClear:
		interval.Classification = CPUIntervalClear
	case outcomeUnknown, outcomeCritical, outcomeSkipped:
	}
	return interval
}

// copyHostCPUDiagnostics validates representation only. The evaluator alone
// decides pressure; invalid optional evidence never changes that decision.
func copyHostCPUDiagnostics(source *HostCPUDiagnostics) *HostCPUDiagnostics {
	if source == nil || (source.CounterUnit != CPUCounterTicks && source.CounterUnit != CPUCounterSeconds) ||
		source.BusyThreshold != cpuBusyThreshold || source.RequiredStreak != requiredStreak ||
		len(source.Intervals) == 0 || len(source.Intervals) > requiredStreak {
		return nil
	}
	copy := *source
	copy.Intervals = slices.Clone(source.Intervals)
	for i, interval := range source.Intervals {
		if !validHostCPUInterval(interval) {
			return nil
		}
		copy.Intervals[i].BusyDelta = copyNumber(interval.BusyDelta)
		copy.Intervals[i].TotalDelta = copyNumber(interval.TotalDelta)
		copy.Intervals[i].BusyFraction = copyNumber(interval.BusyFraction)
	}
	return &copy
}

func copyNumber(number *float64) *float64 {
	if number == nil {
		return nil
	}
	copy := *number
	return &copy
}

func validHostCPUInterval(interval HostCPUInterval) bool {
	if interval.ElapsedMilliseconds < 0 || !optionalFinite(interval.BusyDelta) ||
		!optionalFinite(interval.TotalDelta) || !optionalFinite(interval.BusyFraction) {
		return false
	}
	if interval.BusyFraction != nil && (*interval.BusyFraction < 0 || *interval.BusyFraction > 1) {
		return false
	}
	switch interval.Classification {
	case CPUIntervalClear, CPUIntervalHot:
		return interval.UnavailableReason == "" && validObservedCPUValues(interval)
	case CPUIntervalUnavailable:
		return interval.BusyFraction == nil && validCPUUnavailableReason(interval.UnavailableReason)
	}
	return false
}

func validObservedCPUValues(interval HostCPUInterval) bool {
	return interval.BusyDelta != nil && interval.TotalDelta != nil && interval.BusyFraction != nil &&
		*interval.BusyDelta >= 0 && *interval.TotalDelta > 0 && *interval.BusyDelta <= *interval.TotalDelta
}

func optionalFinite(number *float64) bool {
	return number == nil || (!math.IsNaN(*number) && !math.IsInf(*number, 0))
}

func validCPUUnavailableReason(reason Reason) bool {
	switch reason {
	case ReasonUnreadable, ReasonMalformed, ReasonCounterReset, ReasonNoElapsedTime, ReasonDeadline, ReasonUnsupportedPlatform:
		return true
	case ReasonTargetUnavailable, ReasonTargetChanged, ReasonScopeUnresolved, ReasonCgroupUnsupported, ReasonPressureUnavailable, reasonNotApplicable:
		return false
	}
	return false
}
