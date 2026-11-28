package policy

import (
	"errors"
	"testing"
	"time"
)

func TestParseExecutionBudgetAcceptsFiniteAndLiteralUnlimited(t *testing.T) {
	cases := []struct {
		text      string
		canonical string
		nanos     int64
		finite    time.Duration
		unlimited bool
	}{
		{text: "90s", canonical: "1m30s", nanos: int64(90 * time.Second), finite: 90 * time.Second},
		{text: "1ns", canonical: "1ns", nanos: 1, finite: time.Nanosecond},
		{text: "+2h", canonical: "2h0m0s", nanos: int64(2 * time.Hour), finite: 2 * time.Hour},
		{text: "unlimited", canonical: "unlimited", unlimited: true},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			budget, err := ParseExecutionBudget(tc.text)
			if err != nil {
				t.Fatal(err)
			}
			duration, finite := budget.Finite()
			if budget.IsZero() || budget.Unlimited() != tc.unlimited || finite == tc.unlimited || duration != tc.finite {
				t.Fatalf("budget kind = zero %t unlimited %t finite %v/%t", budget.IsZero(), budget.Unlimited(), duration, finite)
			}
			if budget.String() != tc.canonical || budget.Nanos() != tc.nanos {
				t.Fatalf("representation = %q/%d, want %q/%d", budget.String(), budget.Nanos(), tc.canonical, tc.nanos)
			}
			again, err := ParseExecutionBudget(budget.String())
			if err != nil || again != budget {
				t.Fatalf("canonical text did not parse to the same value: %v, %v", again, err)
			}
		})
	}
}

// Omission is not unlimited and has no default; malformed and nonpositive
// text is invalid but not an omission.
func TestParseExecutionBudgetDistinguishesOmissionFromInvalid(t *testing.T) {
	budget, err := ParseExecutionBudget("")
	if !errors.Is(err, ErrBudgetRequired) || !errors.Is(err, ErrInvalidBudget) || !budget.IsZero() {
		t.Fatalf("omitted budget = %v, %v; want zero ErrBudgetRequired", budget, err)
	}
	for _, text := range []string{"0s", "-1m", "soon", "Unlimited", "UNLIMITED", " unlimited", "unlimited ", "infinite", "inf", "999999999999999999999999h"} {
		t.Run(text, func(t *testing.T) {
			budget, err := ParseExecutionBudget(text)
			if !errors.Is(err, ErrInvalidBudget) || errors.Is(err, ErrBudgetRequired) || !budget.IsZero() {
				t.Fatalf("ParseExecutionBudget(%q) = %v, %v; want invalid, not omitted", text, budget, err)
			}
		})
	}
}

func TestExecutionBudgetConstructorsAndZeroValue(t *testing.T) {
	for _, duration := range []time.Duration{0, -time.Second} {
		if budget, err := FiniteExecutionBudget(duration); !errors.Is(err, ErrInvalidBudget) || !budget.IsZero() {
			t.Fatalf("FiniteExecutionBudget(%v) = %v, %v", duration, budget, err)
		}
	}
	var zero ExecutionBudget
	if !zero.IsZero() || zero.Unlimited() || zero.String() != "" || zero.Nanos() != 0 {
		t.Fatalf("zero budget is not the unsupplied value: %+v", zero)
	}
	if _, finite := zero.Finite(); finite {
		t.Fatal("zero budget reported a finite bound")
	}
	unlimited := UnlimitedExecutionBudget()
	finite, err := FiniteExecutionBudget(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if unlimited == zero || unlimited == finite || finite == zero {
		t.Fatal("distinct budgets compared equal")
	}
}

func TestExecutionBudgetEncloses(t *testing.T) {
	finite, err := FiniteExecutionBudget(2 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !finite.Encloses(2*time.Minute) || finite.Encloses(2*time.Minute+time.Nanosecond) {
		t.Fatal("finite budget bound is not inclusive and exact")
	}
	if !UnlimitedExecutionBudget().Encloses(1000 * time.Hour) {
		t.Fatal("unlimited budget imposed an enclosing bound")
	}
	if (ExecutionBudget{}).Encloses(time.Nanosecond) {
		t.Fatal("unsupplied budget enclosed a duration")
	}
}

func TestLegacyOmittedBudgetKeepsReleasedOmissionMeaning(t *testing.T) {
	cases := []struct {
		provider string
		want     time.Duration
	}{
		{provider: ProviderCodexReview, want: time.Hour},
		{provider: ProviderCodexExec, want: 30 * time.Minute},
		{provider: ProviderClaudePrint, want: 30 * time.Minute},
		{provider: "", want: 30 * time.Minute},
	}
	for _, tc := range cases {
		duration, finite := LegacyOmittedBudget(tc.provider).Finite()
		if !finite || duration != tc.want {
			t.Errorf("LegacyOmittedBudget(%q) = %v/%t, want %v", tc.provider, duration, finite, tc.want)
		}
	}
}

func TestFreshBudgetOmissionCannotSelectLegacySchedulePolicy(t *testing.T) {
	for _, provider := range []string{ProviderCodexExec, ProviderCodexReview} {
		if _, err := ResolveExecutionBudget(provider, "", ExplicitBudget); !errors.Is(err, ErrBudgetRequired) {
			t.Fatalf("fresh %s omission: %v", provider, err)
		}
	}
	for _, tc := range []struct {
		provider string
		want     time.Duration
	}{
		{ProviderCodexExec, 30 * time.Minute}, {ProviderCodexReview, time.Hour},
	} {
		budget, err := ResolveExecutionBudget(tc.provider, "", LegacyScheduleBudget)
		duration, finite := budget.Finite()
		if err != nil || !finite || duration != tc.want {
			t.Fatalf("stored schedule %s: %v/%v", tc.provider, budget, err)
		}
	}
	if _, err := ResolveExecutionBudget(ProviderCodexExec, "1m", BudgetIntent(255)); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("unknown intent: %v", err)
	}
}
