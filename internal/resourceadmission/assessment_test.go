package resourceadmission

import (
	"context"
	"testing"
	"time"
)

func TestAssessmentStatusPrecedence(t *testing.T) {
	pressure := []Pressure{{Metric: MetricHostCPU, Basis: BasisSustained}}
	missing := []Unavailable{{Metric: MetricHostMemory, Reason: ReasonUnreadable}}
	cases := []struct {
		name     string
		findings Findings
		want     Status
	}{
		{"nothing", Findings{}, StatusClear},
		{"advisory only", Findings{Advisories: []Advisory{AdvisoryMemoryHighNear}}, StatusClear},
		{"missing only", Findings{Unavailable: missing}, StatusUnknown},
		{"pressure only", Findings{Pressures: pressure}, StatusPressure},
		{"pressure beats missing", Findings{Pressures: pressure, Unavailable: missing}, StatusPressure},
	}
	for _, tc := range cases {
		if got := NewAssessment(time.Time{}, Scope{}, tc.findings).Status(); got != tc.want {
			t.Errorf("%s: status = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestAssessmentIsImmutable(t *testing.T) {
	input := Findings{Pressures: []Pressure{{Metric: MetricHostCPU, Basis: BasisSustained}}}
	a := NewAssessment(time.Time{}, Scope{}, input)
	input.Pressures[0].Metric = MetricHostMemory
	a.Pressures()[0].Metric = MetricCgroupCPU
	if got := a.Pressures()[0].Metric; got != MetricHostCPU {
		t.Fatalf("metric = %s, want the stored value untouched", got)
	}
}

func TestAssessmentReusableWithinFiveSecondsForTheSameScope(t *testing.T) {
	observedAt := time.Unix(100, 0)
	scope := Scope{TargetIdentity: "a", Cgroup: "/x"}
	a := NewAssessment(observedAt, scope, Findings{})
	cases := []struct {
		name  string
		now   time.Time
		scope Scope
		want  bool
	}{
		{"same instant", observedAt, scope, true},
		{"edge of window", observedAt.Add(5 * time.Second), scope, true},
		{"just past window", observedAt.Add(5*time.Second + time.Nanosecond), scope, false},
		{"clock went backwards", observedAt.Add(-time.Nanosecond), scope, false},
		{"target changed", observedAt, Scope{TargetIdentity: "b", Cgroup: "/x"}, false},
		{"cgroup membership changed", observedAt, Scope{TargetIdentity: "a", Cgroup: "/y"}, false},
	}
	for _, tc := range cases {
		if got := a.Reusable(tc.now, tc.scope); got != tc.want {
			t.Errorf("%s: Reusable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCheckerScope(t *testing.T) {
	t.Run("resolved target and membership", func(t *testing.T) {
		checker, obs, _ := newTestChecker(t, healthySnapshots(1))
		obs.path = observed("/system.slice/job.scope")
		got, err := checker.Scope(context.Background(), steadyResolver())
		want := Scope{TargetIdentity: testTarget.Identity, Cgroup: "/system.slice/job.scope"}
		if err != nil || got != want {
			t.Fatalf("scope = %+v err = %v, want %+v", got, err, want)
		}
	})
	t.Run("unreadable membership keeps identity", func(t *testing.T) {
		checker, obs, _ := newTestChecker(t, healthySnapshots(1))
		obs.path = unobserved[string](ReasonScopeUnresolved)
		got, err := checker.Scope(context.Background(), steadyResolver())
		if err != nil || got != (Scope{TargetIdentity: testTarget.Identity}) {
			t.Fatalf("scope = %+v err = %v", got, err)
		}
	})
	t.Run("no resolver", func(t *testing.T) {
		checker, _, _ := newTestChecker(t, healthySnapshots(1))
		got, err := checker.Scope(context.Background(), nil)
		if err != nil || got != (Scope{}) {
			t.Fatalf("scope = %+v err = %v, want zero", got, err)
		}
	})
	t.Run("caller cancellation", func(t *testing.T) {
		checker, _, _ := newTestChecker(t, healthySnapshots(1))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := checker.Scope(ctx, steadyResolver()); err == nil {
			t.Fatal("want the caller's context error")
		}
	})
}

func TestAssessmentScopeMatchesScopeForTheSameTarget(t *testing.T) {
	ticks := healthySnapshots(4)
	for i := range ticks {
		ticks[i].cgroup = cgroupAt(observed(uint64(0)), memoryLevel(64*gib, gib))
	}
	checker, obs, _ := newTestChecker(t, ticks)
	obs.path = observed(cgroupPathA)
	assessment, err := checker.Check(context.Background(), steadyResolver())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := checker.Scope(context.Background(), steadyResolver())
	if err != nil {
		t.Fatal(err)
	}
	if !assessment.Reusable(assessment.ObservedAt(), scope) {
		t.Fatalf("assessment scope %+v does not cover %+v", assessment.Scope(), scope)
	}
}

func TestZeroAssessmentIsUnknownNeverHealthy(t *testing.T) {
	var zero Assessment
	if zero.Status() != StatusUnknown {
		t.Fatalf("status = %s, want unknown for an assessment that was never observed", zero.Status())
	}
	if zero.Reusable(time.Time{}, Scope{}) {
		t.Fatal("a zero assessment must never be reusable")
	}
}
