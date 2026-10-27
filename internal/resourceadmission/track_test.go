package resourceadmission

import "testing"

func record(t *track, outcomes ...outcome) {
	for _, o := range outcomes {
		t.record(o)
	}
}

func TestTrackConfirmsOnlyOnThreeConsecutiveHotOutcomes(t *testing.T) {
	cases := []struct {
		name     string
		outcomes []outcome
		want     bool
	}{
		{"three hot", []outcome{hotOutcome, hotOutcome, hotOutcome}, true},
		{"two hot", []outcome{hotOutcome, hotOutcome}, false},
		{"clear resets", []outcome{hotOutcome, hotOutcome, clearOutcome, hotOutcome}, false},
		{"unknown resets", []outcome{hotOutcome, hotOutcome, unknownOutcome(ReasonUnreadable), hotOutcome}, false},
		{"streak restarts then confirms", []outcome{hotOutcome, clearOutcome, hotOutcome, hotOutcome, hotOutcome}, true},
	}
	for _, tc := range cases {
		var tr track
		record(&tr, tc.outcomes...)
		if tr.confirmed() != tc.want {
			t.Errorf("%s: confirmed = %v, want %v", tc.name, tr.confirmed(), tc.want)
		}
	}
}

func TestTrackCriticalIsImmediate(t *testing.T) {
	var tr track
	tr.record(criticalOutcome)
	if !tr.confirmed() || !tr.critical {
		t.Fatalf("track = %+v, want an immediate critical confirmation", tr)
	}
}

func TestTrackSkippedOutcomeIsNotRecorded(t *testing.T) {
	var tr track
	tr.record(skippedOutcome)
	if tr.seen {
		t.Fatal("a skipped metric must not become part of the findings")
	}
}

func TestTrackUnknownIsRememberedPastLaterClearSamples(t *testing.T) {
	var tr track
	record(&tr, unknownOutcome(ReasonCounterReset), clearOutcome, clearOutcome)
	if tr.confirmed() || tr.unknown != ReasonCounterReset {
		t.Fatalf("track = %+v, want unavailable counter_reset", tr)
	}
}

func TestTrackExpireKeepsConfirmedPressureAndMarksTheRest(t *testing.T) {
	var confirmed, unfinished, unseen track
	confirmed.record(criticalOutcome)
	record(&unfinished, clearOutcome)
	confirmed.expire(ReasonDeadline)
	unfinished.expire(ReasonDeadline)
	unseen.expire(ReasonDeadline)
	if !confirmed.confirmed() || confirmed.unknown != "" {
		t.Errorf("confirmed = %+v, want pressure kept", confirmed)
	}
	if unfinished.unknown != ReasonDeadline {
		t.Errorf("unfinished = %+v, want deadline", unfinished)
	}
	if unseen.seen || unseen.unknown != "" {
		t.Errorf("unseen = %+v, want untouched", unseen)
	}
}

func TestTrackInvalidateDropsConfirmedPressure(t *testing.T) {
	var tr track
	record(&tr, hotOutcome, hotOutcome, hotOutcome)
	tr.invalidate(ReasonTargetChanged)
	if tr.confirmed() || tr.unknown != ReasonTargetChanged || !tr.seen {
		t.Fatalf("track = %+v, want unavailable target_changed", tr)
	}
}

func TestOutcomeCombinators(t *testing.T) {
	unknown := unknownOutcome(ReasonUnreadable)
	cases := []struct {
		name string
		got  outcome
		want outcomeState
	}{
		{"any: hot wins over unknown", anyOf([]outcome{unknown, hotOutcome}), outcomeHot},
		{"any: unknown over clear", anyOf([]outcome{clearOutcome, unknown}), outcomeUnknown},
		{"any: all clear", anyOf([]outcome{clearOutcome, clearOutcome}), outcomeClear},
		{"any: empty is clear", anyOf(nil), outcomeClear},
		{"all: known clear decides over unknown", allOf(unknown, clearOutcome), outcomeClear},
		{"all: known clear decides first", allOf(clearOutcome, unknown), outcomeClear},
		{"all: clear and hot", allOf(clearOutcome, hotOutcome), outcomeClear},
		{"all: hot and unknown is unknown", allOf(hotOutcome, unknown), outcomeUnknown},
		{"all: both hot", allOf(hotOutcome, hotOutcome), outcomeHot},
	}
	for _, tc := range cases {
		if tc.got.state != tc.want {
			t.Errorf("%s: state = %d, want %d", tc.name, tc.got.state, tc.want)
		}
	}
}

func TestAllOfKeepsTheMissingSideVisibleBesideAKnownClear(t *testing.T) {
	unknown := unknownOutcome(ReasonPressureUnavailable)
	for _, got := range []outcome{allOf(unknown, clearOutcome), allOf(clearOutcome, unknown)} {
		if got.state != outcomeClear || got.reason != ReasonPressureUnavailable {
			t.Fatalf("outcome = %+v, want clear carrying pressure_unavailable", got)
		}
	}
	if got := allOf(clearOutcome, hotOutcome); got.reason != "" {
		t.Fatalf("outcome = %+v, want a plain clear", got)
	}
}

func TestTrackClearWithReasonIsRememberedAsUnavailable(t *testing.T) {
	var tr track
	tr.record(outcome{state: outcomeClear, reason: ReasonPressureUnavailable})
	if tr.confirmed() || tr.unknown != ReasonPressureUnavailable {
		t.Fatalf("track = %+v, want unavailable with no pressure", tr)
	}
}

func TestTrackBaselineHotIsPendingButNotCounted(t *testing.T) {
	var tr track
	tr.baseline(hotOutcome)
	if !tr.pending() || tr.streak != 0 {
		t.Fatalf("track = %+v, want pending without a counted interval", tr)
	}
	record(&tr, hotOutcome, hotOutcome)
	if tr.confirmed() {
		t.Fatal("a baseline warning plus two intervals must not confirm")
	}
	tr.record(hotOutcome)
	if !tr.confirmed() {
		t.Fatal("three intervals after the baseline must confirm")
	}
}

func TestTrackBaselineCriticalIsImmediateAndHealthyIsNotPending(t *testing.T) {
	var critical, healthy track
	critical.baseline(criticalOutcome)
	healthy.baseline(clearOutcome)
	if !critical.confirmed() || healthy.pending() {
		t.Fatalf("critical = %+v healthy = %+v", critical, healthy)
	}
}

func TestTrackClearedSpikeIsNoLongerPending(t *testing.T) {
	var tr track
	record(&tr, hotOutcome, clearOutcome)
	if tr.pending() {
		t.Fatal("a cleared spike must not hold the check open")
	}
}
