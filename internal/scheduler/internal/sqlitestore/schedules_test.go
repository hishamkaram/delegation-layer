package sqlitestore

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestScheduleRoundTripsAndIDsConflict(t *testing.T) {
	store, _ := initializedStore(t)
	definition := testDefinition(testScheduleID)
	createSchedule(t, store, testScheduleID)
	want, err := contract.InitialSchedule(definition)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSchedule(testContext(t), testScheduleID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("get schedule = %+v, %v; want %+v", got, err, want)
	}
	changed := testDefinition(testScheduleID)
	changed.Brief = []byte("a different brief")
	if err := store.CreateSchedule(testContext(t), changed); !errors.Is(err, contract.ErrConflict) {
		t.Fatalf("duplicate schedule err=%v, want ErrConflict", err)
	}
	if again, err := store.GetSchedule(testContext(t), testScheduleID); err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("conflicting create changed the schedule: %+v, %v", again, err)
	}
	if _, err := store.GetSchedule(testContext(t), otherSchedule); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing schedule err=%v, want fs.ErrNotExist", err)
	}
	if _, err := store.GetSchedule(testContext(t), "../not-an-id"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("invalid schedule id err=%v, want a validation error", err)
	}
	invalid := testDefinition(otherSchedule)
	invalid.Brief = nil
	if err := store.CreateSchedule(testContext(t), invalid); err == nil {
		t.Fatal("invalid definition stored")
	}
}

func TestListSchedulesPagesSummaries(t *testing.T) {
	store, _ := initializedStore(t)
	third := "33333333333333333333333333333333"
	for _, id := range []string{third, testScheduleID, otherSchedule} {
		createSchedule(t, store, id)
	}
	first, err := store.ListSchedules(testContext(t), contract.SchedulePage{Limit: 2})
	if err != nil || !first.More || len(first.Schedules) != 2 {
		t.Fatalf("first page %+v, %v", first, err)
	}
	definition := testDefinition(testScheduleID)
	digests, err := contract.DefinitionDigests(definition)
	if err != nil {
		t.Fatal(err)
	}
	want := contract.ScheduleSummary{
		Header: definition.DefinitionHeader, Digests: digests,
		BriefLength: int64(len(definition.Brief)), State: contract.StateEnabled, Cursor: at(1_000),
	}
	if !reflect.DeepEqual(first.Schedules[0], want) || first.Schedules[1].Header.ScheduleID != otherSchedule {
		t.Fatalf("summaries %+v, want first %+v", first.Schedules, want)
	}
	second, err := store.ListSchedules(testContext(t), contract.SchedulePage{AfterScheduleID: otherSchedule, Limit: 2})
	if err != nil || second.More || len(second.Schedules) != 1 || second.Schedules[0].Header.ScheduleID != third {
		t.Fatalf("second page %+v, %v", second, err)
	}
	for _, limit := range []int{0, contract.MaxPageLimit + 1} {
		if _, err := store.ListSchedules(testContext(t), contract.SchedulePage{Limit: limit}); err == nil {
			t.Errorf("limit %d accepted", limit)
		}
	}
}

func TestListSchedulesNeverLoadsTheBrief(t *testing.T) {
	store, root := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	// Same-length brief tampering is caught only by a full read, which proves
	// the list never loaded the brief.
	bypassTrigger(t, root, "schedules_definition_immutable", "UPDATE schedules SET brief = CAST('SUMMARIZE OPEN ISSUES' AS BLOB)")
	if _, err := store.ListSchedules(testContext(t), contract.SchedulePage{Limit: 1}); err != nil {
		t.Fatalf("list read the brief: %v", err)
	}
	if _, err := store.GetSchedule(testContext(t), testScheduleID); !errors.Is(err, task.ErrInvariantFault) {
		t.Fatalf("tampered brief err=%v, want ErrInvariantFault", err)
	}
}

func TestStoredScheduleTamperingIsAnInvariantFault(t *testing.T) {
	cases := map[string]func(t *testing.T, root string){
		"header": func(t *testing.T, root string) {
			bypassTrigger(t, root, "schedules_definition_immutable", "UPDATE schedules SET cron = '* * * * *'")
		},
		"request": func(t *testing.T, root string) {
			bypassTrigger(t, root, "schedules_definition_immutable", "UPDATE schedules SET request_json = CAST('{}' AS BLOB)")
		},
		"state": func(t *testing.T, root string) {
			bypassChecks(t, root, "UPDATE schedules SET state = 'archived'")
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			store, root := initializedStore(t)
			createSchedule(t, store, testScheduleID)
			tamper(t, root)
			if _, err := store.GetSchedule(testContext(t), testScheduleID); !errors.Is(err, task.ErrInvariantFault) {
				t.Errorf("get err=%v, want ErrInvariantFault", err)
			}
			if _, err := store.ListSchedules(testContext(t), contract.SchedulePage{Limit: 1}); !errors.Is(err, task.ErrInvariantFault) {
				t.Errorf("list err=%v, want ErrInvariantFault", err)
			}
		})
	}
}

func TestChangeStateIsACompareAndSet(t *testing.T) {
	store, _ := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	pause := contract.StateChange{ScheduleID: testScheduleID, From: contract.StateEnabled, To: contract.StatePaused}
	if err := store.ChangeState(testContext(t), pause); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := store.ChangeState(testContext(t), pause); !errors.Is(err, contract.ErrConflict) {
		t.Fatalf("stale pause err=%v, want ErrConflict", err)
	}
	stale := contract.StateChange{
		ScheduleID: testScheduleID, From: contract.StatePaused, To: contract.StateEnabled,
		Resume: &contract.Resume{ExpectedCursor: at(900), From: at(900), To: at(2_000)},
	}
	if err := store.ChangeState(testContext(t), stale); !errors.Is(err, contract.ErrConflict) {
		t.Fatalf("stale resume err=%v, want ErrConflict", err)
	}
	requireSchedule(t, store, contract.StatePaused, 1_000)
	requireHistoryLength(t, store, 0)
	resume := contract.StateChange{
		ScheduleID: testScheduleID, From: contract.StatePaused, To: contract.StateEnabled,
		Resume: &contract.Resume{ExpectedCursor: at(1_000), From: at(1_000), To: at(2_000)},
	}
	if err := store.ChangeState(testContext(t), resume); err != nil {
		t.Fatalf("resume: %v", err)
	}
	requireSchedule(t, store, contract.StateEnabled, 2_000)
	requireHistoryLength(t, store, 1)
	remove := contract.StateChange{ScheduleID: testScheduleID, From: contract.StateEnabled, To: contract.StateDeleted}
	if err := store.ChangeState(testContext(t), remove); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.ChangeState(testContext(t), remove); !errors.Is(err, contract.ErrConflict) {
		t.Fatalf("repeated delete err=%v, want ErrConflict", err)
	}
	requireSchedule(t, store, contract.StateDeleted, 2_000)
	missing := contract.StateChange{ScheduleID: otherSchedule, From: contract.StateEnabled, To: contract.StatePaused}
	if err := store.ChangeState(testContext(t), missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing schedule err=%v, want fs.ErrNotExist", err)
	}
}

func requireSchedule(t *testing.T, store *Store, state contract.State, cursor int64) {
	t.Helper()
	schedule, err := store.GetSchedule(testContext(t), testScheduleID)
	if err != nil || schedule.State != state || !schedule.Cursor.Equal(at(cursor)) {
		t.Fatalf("schedule state %s cursor %v err %v; want %s at %v", schedule.State, schedule.Cursor, err, state, at(cursor))
	}
}

func requireHistoryLength(t *testing.T, store *Store, want int) {
	t.Helper()
	page, err := store.History(testContext(t), contract.HistoryPage{ScheduleID: testScheduleID, Limit: contract.MaxPageLimit})
	if err != nil || len(page.Entries) != want {
		t.Fatalf("history %+v, %v; want %d entries", page, err, want)
	}
}
