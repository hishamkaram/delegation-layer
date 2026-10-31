package contract

import (
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSkipReasonsAreClosed(t *testing.T) {
	for _, reason := range []SkipReason{SkipOverlapActive, SkipBlockedUncertain, SkipMissedUnavailable, SkipMissedCapacity, SkipPaused} {
		if err := ValidateSkipReason(reason); err != nil {
			t.Errorf("%s rejected: %v", reason, err)
		}
	}
	for _, reason := range []SkipReason{"", "resource_pressure", "Paused", "missed"} {
		if err := ValidateSkipReason(reason); !errors.Is(err, task.ErrInvalidEnum) {
			t.Errorf("%q: err=%v, want ErrInvalidEnum", reason, err)
		}
	}
}

func TestValidateSkipRequiresForwardSpanFromExpectedCursor(t *testing.T) {
	valid := Skip{ScheduleID: testScheduleID, ExpectedCursor: at(100), Span: SkipSpan{From: at(100), To: at(160), Reason: SkipMissedUnavailable}}
	if err := ValidateSkip(valid); err != nil {
		t.Fatalf("valid skip rejected: %v", err)
	}
	gap := valid
	gap.Span.From = at(130)
	if err := ValidateSkip(gap); err != nil {
		t.Fatalf("skip starting after the cursor rejected: %v", err)
	}
	cases := map[string]func(*Skip){
		"before cursor":   func(s *Skip) { s.Span.From = at(99) },
		"empty span":      func(s *Skip) { s.Span.To = s.Span.From },
		"reversed span":   func(s *Skip) { s.Span.To = at(50) },
		"paused reason":   func(s *Skip) { s.Span.Reason = SkipPaused },
		"unknown reason":  func(s *Skip) { s.Span.Reason = "late" },
		"invalid id":      func(s *Skip) { s.ScheduleID = "" },
		"local from":      func(s *Skip) { s.Span.From = time.Unix(100, 0) },
		"overflowing end": func(s *Skip) { s.Span.To = maximumInstant().Add(time.Nanosecond) },
		"zero cursor":     func(s *Skip) { s.ExpectedCursor = time.Time{} },
	}
	for name, mutate := range cases {
		skip := valid
		mutate(&skip)
		if err := ValidateSkip(skip); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestStoredPausedSpanIsValid(t *testing.T) {
	if err := ValidateSkipSpan(SkipSpan{From: at(1), To: at(2), Reason: SkipPaused}); err != nil {
		t.Fatalf("stored paused span rejected: %v", err)
	}
}

// A span starts at the cursor the previous claim or span set and ends at the
// cursor it sets, so its start is already accounted for and its end must be
// skipped: a claim at the new cursor is refused.
func TestSkipSpanExcludesItsStartAndCoversTheNewCursor(t *testing.T) {
	previousDue := at(100)
	skip := Skip{ScheduleID: testScheduleID, ExpectedCursor: previousDue, Span: SkipSpan{From: previousDue, To: at(160), Reason: SkipOverlapActive}}
	if err := ValidateSkip(skip); err != nil {
		t.Fatalf("skip rejected: %v", err)
	}
	span := skip.Span
	probes := []struct {
		name    string
		due     time.Time
		covered bool
	}{
		{name: "previously claimed start", due: previousDue},
		{name: "just after start", due: previousDue.Add(time.Nanosecond), covered: true},
		{name: "new cursor", due: span.To, covered: true},
		{name: "just after new cursor", due: span.To.Add(time.Nanosecond)},
	}
	for _, probe := range probes {
		if got := span.Covers(probe.due); got != probe.covered {
			t.Errorf("%s: covered=%v, want %v", probe.name, got, probe.covered)
		}
	}
	atCursor := Claim{ScheduleID: testScheduleID, ExpectedCursor: span.To, Due: span.To, TaskID: testTaskID, ArmedAt: span.To}
	if err := ValidateClaim(atCursor); err == nil {
		t.Fatal("claim at the skipped new cursor accepted")
	}
	atCursor.Due = span.To.Add(time.Nanosecond)
	if err := ValidateClaim(atCursor); err != nil {
		t.Fatalf("claim just after the new cursor rejected: %v", err)
	}
}
