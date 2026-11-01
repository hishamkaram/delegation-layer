package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func seedScheduleHistory(t *testing.T, store contract.Store, definition contract.Definition) (contract.Occurrence, time.Time, contract.Occurrence) {
	t.Helper()
	due := definition.CreatedAt.Add(time.Minute)
	first := contract.Claim{ScheduleID: definition.ScheduleID, ExpectedCursor: definition.CreatedAt, Due: due, TaskID: strings.Repeat("b", 32), ArmedAt: due}
	if _, err := store.ClaimAndArm(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	observation := contract.Observation{ObservedAt: due.Add(time.Second), Admission: task.AdmissionAdmitted, Liveness: task.LivenessEnded, Publication: task.PublicationRejected, OutcomeSHA256: strings.Repeat("d", 64), FaultCode: "evidence_fault"}
	if err := store.RecordObservation(t.Context(), contract.OccurrenceObservation{TaskID: first.TaskID, Observation: observation}); err != nil {
		t.Fatal(err)
	}
	if err := store.Release(t.Context(), contract.OccurrenceRelease{TaskID: first.TaskID, Release: contract.Release{ReleasedAt: due.Add(2 * time.Second), Kind: contract.ReleaseClosed, Ref: strings.Repeat("e", 64)}}); err != nil {
		t.Fatal(err)
	}
	end := due.Add(4 * time.Minute)
	if err := store.RecordSkip(t.Context(), contract.Skip{ScheduleID: definition.ScheduleID, ExpectedCursor: due, Span: contract.SkipSpan{From: due, To: end, Reason: contract.SkipMissedUnavailable}}); err != nil {
		t.Fatal(err)
	}
	last := contract.Claim{ScheduleID: definition.ScheduleID, ExpectedCursor: end, Due: end.Add(time.Minute), TaskID: strings.Repeat("c", 32), ArmedAt: end.Add(time.Minute)}
	if _, err := store.ClaimAndArm(t.Context(), last); err != nil {
		t.Fatal(err)
	}
	firstRecord, err := store.GetOccurrence(t.Context(), first.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	lastRecord, err := store.GetOccurrence(t.Context(), last.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	return firstRecord, end, lastRecord
}

func TestScheduleHistoryMixedExclusivePagination(t *testing.T) {
	_, store, definition := scheduleFixture(t)
	first, end, last := seedScheduleHistory(t, store, definition)
	args := Arguments{ScheduleAction: "history", ScheduleID: definition.ScheduleID, ScheduleLimit: 2}
	result := scheduleHistory(t.Context(), store, args, newResponse("schedule"))
	page := requireScheduleHistoryPage(t, result)
	if len(page.Entries) != 2 || page.Entries[0].Occurrence == nil {
		t.Fatalf("mixed page omitted its occurrence: %+v", page)
	}
	if page.Entries[0].Kind != "occurrence" || page.Entries[0].Occurrence.TaskID != last.TaskID || page.Entries[1].Kind != "skip" || page.NextBefore != end.Format(time.RFC3339Nano) {
		t.Fatalf("mixed page: %+v", page)
	}
	span := page.Entries[1].Skip
	if span == nil || !span.From.Equal(first.Due) || !span.To.Equal(end) || span.Reason != contract.SkipMissedUnavailable {
		t.Fatalf("compact interval changed: %+v", span)
	}
	args.ScheduleBefore = page.NextBefore
	older := requireScheduleHistoryPage(t, scheduleHistory(t.Context(), store, args, newResponse("schedule")))
	if len(older.Entries) != 1 || older.NextBefore != "" {
		t.Fatalf("older page: %+v", older)
	}
	assertScheduleHistoryReleasedOccurrence(t, older.Entries[0].Occurrence, first)
	assertScheduleHistoryWire(t, result.response)
}

func requireScheduleHistoryPage(t *testing.T, result commandResult) *ScheduleHistoryResponse {
	t.Helper()
	if result.err != nil || result.code != 0 || result.response.ScheduleHistory == nil {
		t.Fatalf("history query failed: %+v", result)
	}
	return result.response.ScheduleHistory
}

func assertScheduleHistoryReleasedOccurrence(t *testing.T, row *ScheduleOccurrenceResponse, first contract.Occurrence) {
	t.Helper()
	if row == nil || row.TaskID != first.TaskID || row.Reservation != "released" || row.Release == nil || row.Release.Ref != first.Release.Ref || row.Observation.Provenance != "cached" || row.Observation.Publication != "rejected" || row.Observation.ObservedAt == nil || !row.Observation.ObservedAt.Equal(first.Observation.ObservedAt) {
		t.Fatalf("cached rejected data lost: %+v", row)
	}
}

func assertScheduleHistoryWire(t *testing.T, response Response) {
	t.Helper()
	var output bytes.Buffer
	if err := writeJSON(&output, response); err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	row := decoded.ScheduleHistory.Entries[0].Occurrence
	if row.Observation.Provenance != "unobserved" || row.Observation.ObservedAt != nil || row.Observation.Liveness != "undetermined" || row.Observation.Publication != "unknown" || row.Reservation != "active" || row.Release != nil {
		t.Fatalf("unobserved occurrence misrepresented: %+v", row)
	}
	output.Reset()
	if err := writeHumanResponse(&output, response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "provenance=unobserved") || !strings.Contains(output.String(), "from_exclusive=") || !strings.Contains(output.String(), "schedule execution "+row.TaskID) {
		t.Fatalf("human history missing semantics: %q", output.String())
	}
}

func TestScheduleHistoryNewerInsertDoesNotRepeatOlderPage(t *testing.T) {
	_, store, definition := scheduleFixture(t)
	first, end, last := seedScheduleHistory(t, store, definition)
	args := Arguments{ScheduleAction: "history", ScheduleID: definition.ScheduleID, ScheduleLimit: 2}
	initial := scheduleHistory(t.Context(), store, args, newResponse("schedule"))
	if initial.err != nil {
		t.Fatal(initial.err)
	}
	newEnd := last.Due.Add(10 * time.Minute)
	if err := store.RecordSkip(t.Context(), contract.Skip{ScheduleID: definition.ScheduleID, ExpectedCursor: last.Due, Span: contract.SkipSpan{From: last.Due, To: newEnd, Reason: contract.SkipOverlapActive}}); err != nil {
		t.Fatal(err)
	}
	args.ScheduleBefore = initial.response.ScheduleHistory.NextBefore
	older := scheduleHistory(t.Context(), store, args, newResponse("schedule"))
	if older.err != nil || len(older.response.ScheduleHistory.Entries) != 1 || older.response.ScheduleHistory.Entries[0].Occurrence.TaskID != first.TaskID {
		t.Fatalf("older page changed after insert: %+v", older)
	}
	args.ScheduleBefore = ""
	newest := scheduleHistory(t.Context(), store, args, newResponse("schedule"))
	if newest.err != nil || newest.response.ScheduleHistory.Entries[0].Skip == nil || !newest.response.ScheduleHistory.Entries[0].Endpoint.Equal(newEnd) || newest.response.ScheduleHistory.NextBefore == end.Format(time.RFC3339Nano) {
		t.Fatalf("new first page does not include newer insert: %+v", newest)
	}
}

func TestScheduleHistoryEmptyMissingAndRetainedStates(t *testing.T) {
	_, store, definition := scheduleFixture(t)
	args := Arguments{ScheduleAction: "history", ScheduleID: definition.ScheduleID, ScheduleLimit: 50}
	empty := scheduleHistory(t.Context(), store, args, newResponse("schedule"))
	if empty.err != nil || empty.response.ScheduleHistory == nil || empty.response.ScheduleHistory.Entries == nil || len(empty.response.ScheduleHistory.Entries) != 0 {
		t.Fatalf("existing empty schedule: %+v", empty)
	}
	seedScheduleHistory(t, store, definition)
	current := contract.StateEnabled
	for _, state := range []contract.State{contract.StatePaused, contract.StateDeleted} {
		if err := store.ChangeState(t.Context(), contract.StateChange{ScheduleID: definition.ScheduleID, From: current, To: state}); err != nil {
			t.Fatal(err)
		}
		result := scheduleHistory(t.Context(), store, args, newResponse("schedule"))
		if result.err != nil || result.code != 0 || len(result.response.ScheduleHistory.Entries) != 3 {
			t.Fatalf("state=%s history=%+v", state, result)
		}
		current = state
	}
	args.ScheduleID = strings.Repeat("f", 32)
	missing := scheduleHistory(t.Context(), store, args, newResponse("schedule"))
	if missing.code != 1 || !errors.Is(missing.err, fs.ErrNotExist) {
		t.Fatalf("missing schedule: %+v", missing)
	}
}

func TestScheduleHistoryResponseRejectsOversizeAuthority(t *testing.T) {
	response := newResponse("schedule")
	response.ScheduleHistory = &ScheduleHistoryResponse{ScheduleID: scheduleTestID, NextBefore: strings.Repeat("x", MaxJSONResponseBytes)}
	var output bytes.Buffer
	if err := writeJSON(&output, response); !errors.Is(err, ErrJSONResponseTooLarge) || output.Len() != 0 {
		t.Fatalf("authoritative cursor truncated or emitted: bytes=%d err=%v", output.Len(), err)
	}
	if len(response.ScheduleHistory.NextBefore) != MaxJSONResponseBytes {
		t.Fatal("failed presentation mutated the authoritative cursor")
	}
}

type scheduleHistoryFaultStore struct {
	contract.Store
	err error
}

func (s scheduleHistoryFaultStore) History(context.Context, contract.HistoryPage) (contract.HistoryPageResult, error) {
	return contract.HistoryPageResult{}, s.err
}

func TestScheduleHistoryFailureRetainsJoinedCause(t *testing.T) {
	_, store, _ := scheduleFixture(t)
	privateFault := errors.New("private lower-level diagnostic")
	args := Arguments{ScheduleAction: "history", ScheduleID: scheduleTestID, ScheduleLimit: 50}
	for _, testCase := range []struct {
		err  error
		code string
	}{
		{err: task.ErrLockBusy, code: "database_busy"},
		{err: errors.Join(fs.ErrNotExist, task.ErrInvariantFault, privateFault), code: "invalid_evidence"},
	} {
		result := scheduleHistory(t.Context(), scheduleHistoryFaultStore{Store: store, err: testCase.err}, args, newResponse("schedule"))
		if result.code != 1 || !errors.Is(result.err, testCase.err) || result.response.ScheduleQueryError == nil || result.response.ScheduleQueryError.Code != testCase.code || result.response.ScheduleHistory.Entries != nil {
			t.Fatalf("query failure hidden or became empty success: %+v", result)
		}
		var output bytes.Buffer
		if err := writeJSONResponse(&output, "schedule", &result.response, result.err); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), privateFault.Error()) {
			t.Fatal("query exposed internal failure diagnostics")
		}
	}
}
