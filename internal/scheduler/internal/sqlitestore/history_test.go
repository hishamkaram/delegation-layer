package sqlitestore

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// mixedHistory builds, oldest first: occurrence due 2000, span (2000, 3000],
// occurrence due 4000, then a paused span (4000, 5000] from a resume.
func mixedHistory(t *testing.T) (*Store, string) {
	t.Helper()
	store, root := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	claimed(t, store, testClaim(testScheduleID, testTaskID, 1_000, 2_000))
	if err := store.Release(testContext(t), testRelease(testTaskID, 2_100)); err != nil {
		t.Fatal(err)
	}
	skip := contract.Skip{
		ScheduleID: testScheduleID, ExpectedCursor: at(2_000),
		Span: contract.SkipSpan{From: at(2_000), To: at(3_000), Reason: contract.SkipMissedCapacity},
	}
	if err := store.RecordSkip(testContext(t), skip); err != nil {
		t.Fatalf("skip: %v", err)
	}
	claimed(t, store, testClaim(testScheduleID, otherTaskID, 3_000, 4_000))
	pause := contract.StateChange{ScheduleID: testScheduleID, From: contract.StateEnabled, To: contract.StatePaused}
	if err := store.ChangeState(testContext(t), pause); err != nil {
		t.Fatal(err)
	}
	resume := contract.StateChange{
		ScheduleID: testScheduleID, From: contract.StatePaused, To: contract.StateEnabled,
		Resume: &contract.Resume{ExpectedCursor: at(4_000), From: at(4_000), To: at(5_000)},
	}
	if err := store.ChangeState(testContext(t), resume); err != nil {
		t.Fatal(err)
	}
	return store, root
}

func TestHistoryPagesMixedEntriesByExclusiveEndpoint(t *testing.T) {
	store, _ := mixedHistory(t)
	endpoints, kinds := collectHistory(t, store, contract.HistoryPage{ScheduleID: testScheduleID, Limit: 2})
	wantEndpoints := []int64{5_000, 4_000, 3_000, 2_000}
	wantKinds := []string{"skip", "occurrence", "skip", "occurrence"}
	if len(endpoints) != len(wantEndpoints) {
		t.Fatalf("endpoints %v, want %v", endpoints, wantEndpoints)
	}
	for i := range wantEndpoints {
		if endpoints[i] != wantEndpoints[i] || kinds[i] != wantKinds[i] {
			t.Fatalf("entries %v %v, want %v %v", endpoints, kinds, wantEndpoints, wantKinds)
		}
	}
	before := at(4_000)
	below, err := store.History(testContext(t), contract.HistoryPage{ScheduleID: testScheduleID, Before: &before, Limit: 1})
	if err != nil || !below.More || len(below.Entries) != 1 {
		t.Fatalf("page below 4000 = %+v, %v", below, err)
	}
	if span, isSkip := below.Entries[0].Skip(); !isSkip || !span.To.Equal(at(3_000)) {
		t.Fatalf("exclusive before returned %+v", below.Entries[0])
	}
	if _, err := store.History(testContext(t), contract.HistoryPage{ScheduleID: otherSchedule, Limit: 1}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing schedule history err=%v, want fs.ErrNotExist", err)
	}
}

// collectHistory follows Next from page to the last page and returns every
// entry's endpoint in seconds and its kind.
func collectHistory(t *testing.T, store *Store, page contract.HistoryPage) ([]int64, []string) {
	t.Helper()
	var endpoints []int64
	var kinds []string
	for more := true; more; {
		result, err := store.History(testContext(t), page)
		if err != nil {
			t.Fatalf("history page before %v: %v", page.Before, err)
		}
		for _, entry := range result.Entries {
			endpoint, endpointErr := entry.Endpoint()
			if endpointErr != nil {
				t.Fatal(endpointErr)
			}
			endpoints = append(endpoints, endpoint.Unix())
			kind := "occurrence"
			if _, isSkip := entry.Skip(); isSkip {
				kind = "skip"
			}
			kinds = append(kinds, kind)
		}
		if page, more, err = result.Next(page); err != nil {
			t.Fatal(err)
		}
	}
	return endpoints, kinds
}

func TestRecordSkipIsACompareAndSet(t *testing.T) {
	store, _ := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	stale := contract.Skip{
		ScheduleID: testScheduleID, ExpectedCursor: at(900),
		Span: contract.SkipSpan{From: at(1_000), To: at(2_000), Reason: contract.SkipOverlapActive},
	}
	if err := store.RecordSkip(testContext(t), stale); !errors.Is(err, contract.ErrConflict) {
		t.Fatalf("stale skip err=%v, want ErrConflict", err)
	}
	requireSchedule(t, store, contract.StateEnabled, 1_000)
	requireHistoryLength(t, store, 0)
	paused := contract.Skip{
		ScheduleID: testScheduleID, ExpectedCursor: at(1_000),
		Span: contract.SkipSpan{From: at(1_000), To: at(2_000), Reason: contract.SkipPaused},
	}
	if err := store.RecordSkip(testContext(t), paused); err == nil {
		t.Fatal("paused skip recorded outside resume")
	}
	missing := contract.Skip{
		ScheduleID: otherSchedule, ExpectedCursor: at(1_000),
		Span: contract.SkipSpan{From: at(1_000), To: at(2_000), Reason: contract.SkipBlockedUncertain},
	}
	if err := store.RecordSkip(testContext(t), missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing schedule skip err=%v, want fs.ErrNotExist", err)
	}
}

func TestHistoryRefusesTwoEntriesAtOneEndpoint(t *testing.T) {
	store, root := mixedHistory(t)
	conn := enforcingConn(t, root)
	rawExec(t, conn, "INSERT INTO skip_spans VALUES ('"+testScheduleID+"', 1500000000000, 2000000000000, 'missed_unavailable')")
	closeRaw(t, conn)
	if _, err := store.History(testContext(t), contract.HistoryPage{ScheduleID: testScheduleID, Limit: contract.MaxPageLimit}); !errors.Is(err, task.ErrInvariantFault) {
		t.Fatalf("duplicate endpoint err=%v, want ErrInvariantFault", err)
	}
}
