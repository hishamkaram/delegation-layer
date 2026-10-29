package contract

import (
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestHistoryEntryHoldsExactlyOneKind(t *testing.T) {
	occurrence := OccurrenceEntry(testOccurrence())
	if got, ok := occurrence.Occurrence(); !ok || got.TaskID != testTaskID {
		t.Fatalf("occurrence entry = %+v, %v", got, ok)
	}
	if _, ok := occurrence.Skip(); ok {
		t.Fatal("occurrence entry is also a skip")
	}
	span := SkipSpan{From: at(1_000), To: at(1_500), Reason: SkipMissedCapacity}
	skip := SkipEntry(span)
	if got, ok := skip.Skip(); !ok || got != span {
		t.Fatalf("skip entry = %+v, %v", got, ok)
	}
	if _, ok := skip.Occurrence(); ok {
		t.Fatal("skip entry is also an occurrence")
	}
	var empty HistoryEntry
	_, isOccurrence := empty.Occurrence()
	_, isSkip := empty.Skip()
	if _, err := empty.Endpoint(); err == nil || isOccurrence || isSkip {
		t.Fatal("zero history entry has a kind or endpoint")
	}
	if err := ValidateHistoryEntry(testScheduleID, empty); err == nil {
		t.Fatal("zero history entry accepted")
	}
}

func TestHistoryEntryDoesNotAliasItsOccurrence(t *testing.T) {
	input := testOccurrence()
	input.Observation = &Observation{ObservedAt: at(2_100)}
	input.Release = &Release{ReleasedAt: at(2_200), Kind: ReleaseNotCreated, Ref: "resource_pressure"}
	entry := OccurrenceEntry(input)
	input.Observation.ObservedAt = at(9_999)
	input.Release.Ref = "changed"
	read, _ := entry.Occurrence()
	read.Release.Ref = "mutated"
	again, _ := entry.Occurrence()
	if !again.Observation.ObservedAt.Equal(at(2_100)) || again.Release.Ref != "resource_pressure" {
		t.Fatalf("entry aliased caller memory: %+v %+v", again.Observation, again.Release)
	}
	if err := ValidateHistoryEntry(testScheduleID, entry); err != nil {
		t.Fatalf("released occurrence entry rejected: %v", err)
	}
}

// mixedHistory builds one schedule's history, newest first, from a skip, a
// claim, and a resume that each passed validation against the previous cursor.
func mixedHistory(t *testing.T) []HistoryEntry {
	t.Helper()
	cursor := at(1_000)
	first := Skip{ScheduleID: testScheduleID, ExpectedCursor: cursor, Span: SkipSpan{From: cursor, To: at(1_500), Reason: SkipMissedUnavailable}}
	claim := Claim{ScheduleID: testScheduleID, ExpectedCursor: first.Span.To, Due: at(2_000), TaskID: testTaskID, ArmedAt: at(2_000)}
	resume := Resume{ExpectedCursor: claim.Due, From: claim.Due, To: at(3_000)}
	if err := errors.Join(ValidateSkip(first), ValidateClaim(claim), ValidateStateChange(StateChange{ScheduleID: testScheduleID, From: StatePaused, To: StateEnabled, Resume: &resume})); err != nil {
		t.Fatalf("cursor sequence rejected: %v", err)
	}
	occurrence := testOccurrence()
	occurrence.Due, occurrence.ArmedAt = claim.Due, claim.ArmedAt
	return []HistoryEntry{SkipEntry(resume.Span()), OccurrenceEntry(occurrence), SkipEntry(first.Span)}
}

func TestMixedHistoryEndpointsAreUniqueUnderStrictCursorAdvance(t *testing.T) {
	want := []time.Time{at(3_000), at(2_000), at(1_500)}
	for i, entry := range mixedHistory(t) {
		endpoint, err := entry.Endpoint()
		if err != nil || !endpoint.Equal(want[i]) {
			t.Fatalf("entry %d endpoint %v err=%v, want %v", i, endpoint, err, want[i])
		}
		if err := ValidateHistoryEntry(testScheduleID, entry); err != nil {
			t.Fatalf("entry %d rejected: %v", i, err)
		}
	}
}

func TestMixedHistoryAccountsForEachEndpointOnce(t *testing.T) {
	entries := mixedHistory(t)
	resume, _ := entries[0].Skip()
	claimed, _ := entries[1].Occurrence()
	first, _ := entries[2].Skip()
	if resume.Covers(claimed.Due) || first.Covers(claimed.Due) {
		t.Fatalf("claimed due %v also counted as skipped", claimed.Due)
	}
	if !resume.Covers(resume.To) || !first.Covers(first.To) || first.Covers(first.From) {
		t.Fatal("skip spans must cover their end and exclude their start")
	}
}

func TestHistoryNextPageUsesTheExclusiveLastEndpoint(t *testing.T) {
	newestFirst := mixedHistory(t)
	page := HistoryPage{ScheduleID: testScheduleID, Limit: 2}
	next, ok, err := HistoryPageResult{Entries: newestFirst[:2], More: true}.Next(page)
	if err != nil || !ok || next.ScheduleID != testScheduleID || next.Limit != 2 || !next.Before.Equal(at(2_000)) {
		t.Fatalf("next page %+v ok=%v err=%v", next, ok, err)
	}
	oldest, err := newestFirst[2].Endpoint()
	if err != nil || ValidateHistoryPage(next) != nil || !oldest.Before(*next.Before) {
		t.Fatalf("next page must be valid and still admit the older entry: %v", err)
	}
	if _, ok, err := (HistoryPageResult{Entries: newestFirst[2:]}).Next(next); ok || err != nil {
		t.Fatalf("last page offered another page: ok=%v err=%v", ok, err)
	}
	if _, ok, err := (HistoryPageResult{Entries: []HistoryEntry{{}}, More: true}).Next(page); ok || err == nil {
		t.Fatal("empty entry produced a page key")
	}
}

func TestHistoryRejectsAnotherSchedulesOccurrence(t *testing.T) {
	other := testOccurrence()
	other.ScheduleID = testTaskID
	if err := ValidateHistoryEntry(testScheduleID, OccurrenceEntry(other)); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("foreign occurrence err=%v, want ErrIdentityMismatch", err)
	}
	invalid := testOccurrence()
	invalid.Release = &Release{ReleasedAt: at(1), Kind: ReleaseClosed, Ref: "not-a-digest"}
	if err := ValidateHistoryEntry(testScheduleID, OccurrenceEntry(invalid)); err == nil {
		t.Fatal("invalid occurrence entry accepted")
	}
}

func TestPageRequestsAreBounded(t *testing.T) {
	before := at(5_000)
	local := time.Unix(5_000, 0)
	for _, limit := range []int{1, MaxPageLimit} {
		if err := errors.Join(
			ValidateSchedulePage(SchedulePage{Limit: limit}),
			ValidateSchedulePage(SchedulePage{AfterScheduleID: testScheduleID, Limit: limit}),
			ValidateActivePage(ActivePage{AfterScheduleID: testScheduleID, Limit: limit}),
			ValidateHistoryPage(HistoryPage{ScheduleID: testScheduleID, Before: &before, Limit: limit}),
		); err != nil {
			t.Fatalf("limit %d rejected: %v", limit, err)
		}
	}
	refused := map[string]error{
		"schedule limit zero":    ValidateSchedulePage(SchedulePage{}),
		"schedule limit over":    ValidateSchedulePage(SchedulePage{Limit: MaxPageLimit + 1}),
		"schedule invalid after": ValidateSchedulePage(SchedulePage{AfterScheduleID: "x", Limit: 1}),
		"active negative limit":  ValidateActivePage(ActivePage{Limit: -1}),
		"active invalid after":   ValidateActivePage(ActivePage{AfterScheduleID: "x", Limit: 1}),
		"history missing id":     ValidateHistoryPage(HistoryPage{Limit: 1}),
		"history local before":   ValidateHistoryPage(HistoryPage{ScheduleID: testScheduleID, Before: &local, Limit: 1}),
		"history limit over":     ValidateHistoryPage(HistoryPage{ScheduleID: testScheduleID, Limit: MaxPageLimit + 1}),
	}
	for name, err := range refused {
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
