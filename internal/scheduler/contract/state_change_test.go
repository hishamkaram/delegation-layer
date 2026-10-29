package contract

import (
	"testing"
	"time"
)

func TestValidateStateChangeAllowsOnlyLiveTransitions(t *testing.T) {
	states := []State{StateEnabled, StatePaused, StateDeleted}
	permitted := map[[2]State]bool{
		{StateEnabled, StatePaused}:  true,
		{StateEnabled, StateDeleted}: true,
		{StatePaused, StateEnabled}:  true,
		{StatePaused, StateDeleted}:  true,
	}
	for _, from := range states {
		for _, to := range states {
			err := ValidateStateChange(StateChange{ScheduleID: testScheduleID, From: from, To: to})
			if (err == nil) != permitted[[2]State{from, to}] {
				t.Errorf("%s -> %s: err=%v, want permitted=%v", from, to, err, permitted[[2]State{from, to}])
			}
		}
	}
	for _, change := range []StateChange{
		{ScheduleID: "bad", From: StateEnabled, To: StatePaused},
		{ScheduleID: testScheduleID, From: "archived", To: StatePaused},
		{ScheduleID: testScheduleID, From: StatePaused, To: "running"},
	} {
		if err := ValidateStateChange(change); err == nil {
			t.Errorf("%+v accepted", change)
		}
	}
}

func TestResumeRecordsOnlyAForwardPausedSpan(t *testing.T) {
	valid := Resume{ExpectedCursor: at(100), From: at(100), To: at(200)}
	if span := valid.Span(); span.Reason != SkipPaused || !span.From.Equal(valid.From) || !span.To.Equal(valid.To) {
		t.Fatalf("resume span %+v", span)
	}
	resume := StateChange{ScheduleID: testScheduleID, From: StatePaused, To: StateEnabled, Resume: &valid}
	if err := ValidateStateChange(resume); err != nil {
		t.Fatalf("valid resume rejected: %v", err)
	}
	later := Resume{ExpectedCursor: at(100), From: at(150), To: at(200)}
	if err := ValidateStateChange(StateChange{ScheduleID: testScheduleID, From: StatePaused, To: StateEnabled, Resume: &later}); err != nil {
		t.Fatalf("resume starting after the cursor rejected: %v", err)
	}
	refused := map[string]StateChange{
		"pause with span":    {ScheduleID: testScheduleID, From: StateEnabled, To: StatePaused, Resume: &valid},
		"delete with span":   {ScheduleID: testScheduleID, From: StatePaused, To: StateDeleted, Resume: &valid},
		"resume deleted":     {ScheduleID: testScheduleID, From: StateDeleted, To: StateEnabled, Resume: &valid},
		"empty span":         {ScheduleID: testScheduleID, From: StatePaused, To: StateEnabled, Resume: &Resume{ExpectedCursor: at(100), From: at(200), To: at(200)}},
		"before cursor":      {ScheduleID: testScheduleID, From: StatePaused, To: StateEnabled, Resume: &Resume{ExpectedCursor: at(100), From: at(99), To: at(200)}},
		"local span end":     {ScheduleID: testScheduleID, From: StatePaused, To: StateEnabled, Resume: &Resume{ExpectedCursor: at(100), From: at(100), To: time.Unix(200, 0)}},
		"zero expected time": {ScheduleID: testScheduleID, From: StatePaused, To: StateEnabled, Resume: &Resume{From: at(100), To: at(200)}},
	}
	for name, change := range refused {
		if err := ValidateStateChange(change); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
