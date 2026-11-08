package resourceadmission

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func diagnosticNumber(value float64) *float64 { return &value }

func diagnosticFixture() *HostCPUDiagnostics {
	return &HostCPUDiagnostics{
		CounterUnit: CPUCounterTicks, BusyThreshold: 0.95, RequiredStreak: 3,
		Intervals: []HostCPUInterval{{
			ElapsedMilliseconds: 1000, BusyDelta: diagnosticNumber(95), TotalDelta: diagnosticNumber(100),
			BusyFraction: diagnosticNumber(0.95), Classification: CPUIntervalHot,
		}},
	}
}

func TestCheckHostCPUDiagnosticsUseOnlyConsumedSamples(t *testing.T) {
	// Independent counter arithmetic: (105-10)/(140-40)=.95,
	// (303-105)/(340-140)=.99, (600-303)/(640-340)=.99.
	ticks := healthySnapshots(4)
	for i, counters := range []cpuCounters{{10, 40}, {105, 140}, {303, 340}, {600, 640}} {
		ticks[i].cpu = observed(counters)
	}
	checker, observer, clock := newTestChecker(t, ticks)
	assessment, err := checker.Check(context.Background(), nil)
	if err != nil || pressuresOf(assessment)[MetricHostCPU] != BasisSustained {
		t.Fatalf("pressure=%v err=%v", assessment.Pressures(), err)
	}
	wantUnit := CPUCounterSeconds
	if runtime.GOOS == "linux" {
		wantUnit = CPUCounterTicks
	}
	want := &HostCPUDiagnostics{
		CounterUnit: wantUnit, BusyThreshold: 0.95, RequiredStreak: 3,
		Intervals: []HostCPUInterval{
			{1000, diagnosticNumber(95), diagnosticNumber(100), diagnosticNumber(0.95), CPUIntervalHot, ""},
			{1000, diagnosticNumber(198), diagnosticNumber(200), diagnosticNumber(0.99), CPUIntervalHot, ""},
			{1000, diagnosticNumber(297), diagnosticNumber(300), diagnosticNumber(0.99), CPUIntervalHot, ""},
		},
	}
	if got := assessment.HostCPUDiagnostics(); !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostics=%+v, want %+v", got, want)
	}
	if len(observer.targets) != 4 || observer.memoryCalls != 1 || len(clock.sleeps) != 3 {
		t.Fatalf("unexpected extra observations: ticks=%d prechecks=%d waits=%d", len(observer.targets), observer.memoryCalls, len(clock.sleeps))
	}
}

func TestHostCPUIntervalEvidenceKeepsInvalidNumbersAbsent(t *testing.T) {
	at := func(busy, total float64) reading[cpuCounters] { return observed(cpuCounters{busy: busy, total: total}) }
	for _, testCase := range []struct {
		name      string
		prev, cur reading[cpuCounters]
		reason    Reason
		busy      *float64
		total     *float64
	}{
		{"not a number", at(0, 0), at(math.NaN(), 100), ReasonMalformed, nil, nil},
		{"infinite", at(0, 0), at(1, math.Inf(1)), ReasonMalformed, nil, nil},
		{"busy reset", at(95, 100), at(5, 200), ReasonCounterReset, diagnosticNumber(-90), diagnosticNumber(100)},
		{"total reset", at(5, 100), at(6, 90), ReasonCounterReset, diagnosticNumber(1), diagnosticNumber(-10)},
		{"no progress", at(5, 100), at(5, 100), ReasonNoElapsedTime, diagnosticNumber(0), diagnosticNumber(0)},
		{"invalid delta", at(0, 100), at(9, 102), ReasonMalformed, diagnosticNumber(9), diagnosticNumber(2)},
		{"missing current", at(0, 0), unobserved[cpuCounters](ReasonUnreadable), ReasonUnreadable, nil, nil},
		{"missing baseline", unobserved[cpuCounters](ReasonDeadline), at(95, 100), ReasonDeadline, nil, nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := evaluateHostCPU(testCase.prev, testCase.cur)
			interval := hostCPUInterval(1234*time.Millisecond, result)
			want := HostCPUInterval{1234, testCase.busy, testCase.total, nil, CPUIntervalUnavailable, testCase.reason}
			if !reflect.DeepEqual(interval, want) || result.outcome.state != outcomeUnknown {
				t.Fatalf("interval=%+v outcome=%+v want=%+v", interval, result.outcome, want)
			}
			if _, err := json.Marshal(interval); err != nil {
				t.Fatalf("unavailable evidence must stay JSON-safe: %v", err)
			}
		})
	}
}

func TestHostCPUDiagnosticsAreImmutableAndDoNotOwnTheVerdict(t *testing.T) {
	source := diagnosticFixture()
	findings := Findings{Pressures: []Pressure{{MetricHostCPU, BasisSustained}}, HostCPU: source}
	assessment := NewAssessment(time.Time{}, Scope{}, findings)
	want := assessment.HostCPUDiagnostics()
	source.CounterUnit = CPUCounterSeconds
	*source.Intervals[0].BusyDelta = 3
	*source.Intervals[0].TotalDelta = 4
	*source.Intervals[0].BusyFraction = 0.75
	source.Intervals[0].Classification = CPUIntervalClear
	source.Intervals = append(source.Intervals, HostCPUInterval{})
	returned := assessment.HostCPUDiagnostics()
	returned.BusyThreshold = 0
	*returned.Intervals[0].BusyDelta = 0
	*returned.Intervals[0].TotalDelta = 0
	*returned.Intervals[0].BusyFraction = 0
	returned.Intervals[0].Classification = CPUIntervalUnavailable
	if got := assessment.HostCPUDiagnostics(); !reflect.DeepEqual(got, want) {
		t.Fatalf("evidence mutated through input/output: got=%+v want=%+v", got, want)
	}
	without := findings
	without.HostCPU = nil
	plain := NewAssessment(time.Time{}, Scope{}, without)
	if assessment.Status() != plain.Status() || !reflect.DeepEqual(assessment.Pressures(), plain.Pressures()) ||
		!reflect.DeepEqual(assessment.Unavailable(), plain.Unavailable()) {
		t.Fatal("diagnostic evidence changed the admission findings")
	}
}

func TestHostCPUDiagnosticsRejectUnsafeOrUnboundedEvidence(t *testing.T) {
	const secret = "/private/cpu-canary PID=4242 TOKEN=cpu-secret"
	for name, mutate := range map[string]func(*HostCPUDiagnostics){
		"unit canary":           func(d *HostCPUDiagnostics) { d.CounterUnit = CPUCounterUnit(secret) },
		"classification canary": func(d *HostCPUDiagnostics) { d.Intervals[0].Classification = CPUIntervalClassification(secret) },
		"reason canary": func(d *HostCPUDiagnostics) {
			d.Intervals[0].Classification = CPUIntervalUnavailable
			d.Intervals[0].BusyFraction = nil
			d.Intervals[0].UnavailableReason = Reason(secret)
		},
		"nan delta":         func(d *HostCPUDiagnostics) { d.Intervals[0].BusyDelta = diagnosticNumber(math.NaN()) },
		"infinite delta":    func(d *HostCPUDiagnostics) { d.Intervals[0].TotalDelta = diagnosticNumber(math.Inf(1)) },
		"invalid fraction":  func(d *HostCPUDiagnostics) { d.Intervals[0].BusyFraction = diagnosticNumber(1.01) },
		"missing fraction":  func(d *HostCPUDiagnostics) { d.Intervals[0].BusyFraction = nil },
		"negative elapsed":  func(d *HostCPUDiagnostics) { d.Intervals[0].ElapsedMilliseconds = -1 },
		"changed threshold": func(d *HostCPUDiagnostics) { d.BusyThreshold = 0.5 },
		"changed streak":    func(d *HostCPUDiagnostics) { d.RequiredStreak = 2 },
		"empty intervals":   func(d *HostCPUDiagnostics) { d.Intervals = nil },
		"too many intervals": func(d *HostCPUDiagnostics) {
			d.Intervals = []HostCPUInterval{d.Intervals[0], d.Intervals[0], d.Intervals[0], d.Intervals[0]}
		},
	} {
		t.Run(name, func(t *testing.T) {
			source := diagnosticFixture()
			mutate(source)
			assessment := NewAssessment(time.Time{}, Scope{}, Findings{Pressures: []Pressure{{MetricHostCPU, BasisSustained}}, HostCPU: source})
			if assessment.Status() != StatusPressure || assessment.HostCPUDiagnostics() != nil {
				t.Fatalf("invalid optional evidence changed verdict or escaped: %+v", assessment)
			}
			encoded, err := json.Marshal(assessment.HostCPUDiagnostics())
			if err != nil || strings.Contains(string(encoded), secret) {
				t.Fatalf("unsafe evidence escaped: %s err=%v", encoded, err)
			}
		})
	}
}

func TestHostCPUDiagnosticsOmittedWithoutConfirmedCPUOrAfterCancellation(t *testing.T) {
	for name, findings := range map[string]Findings{
		"clear":           {},
		"unknown":         {Unavailable: []Unavailable{{MetricHostCPU, ReasonUnreadable}}},
		"memory pressure": {Pressures: []Pressure{{MetricHostMemory, BasisCritical}}},
	} {
		findings.HostCPU = diagnosticFixture()
		if got := NewAssessment(time.Time{}, Scope{}, findings).HostCPUDiagnostics(); got != nil {
			t.Errorf("%s exposed CPU evidence: %+v", name, got)
		}
	}
	ticks := withCPU(healthySnapshots(4), cpuSeries(0.99, 0.99, 0.99))
	checker, observer, clock := newTestChecker(t, ticks)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock.onSleep = cancel
	assessment, err := checker.Check(ctx, nil)
	if !errors.Is(err, context.Canceled) || assessment.HostCPUDiagnostics() != nil || len(observer.targets) != 1 {
		t.Fatalf("cancelled check retained evidence or sampled again: assessment=%+v err=%v ticks=%d", assessment, err, len(observer.targets))
	}
}
