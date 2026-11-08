package command

import (
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

func TestResumePreservesCursorWhenClockIsBehind(t *testing.T) {
	cursor := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	current := contract.Schedule{Definition: contract.Definition{DefinitionHeader: contract.DefinitionHeader{ScheduleID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, State: contract.StatePaused, Cursor: cursor}
	change, warning, err := PlanStateChange(current, "resume", cursor.Add(-time.Second))
	if err != nil || change.From != contract.StatePaused || change.To != contract.StateEnabled || change.Resume != nil || warning == nil || warning.Code != "clock_behind_cursor" {
		t.Fatalf("resume behind cursor changed its semantics: %+v %+v %v", change, warning, err)
	}
	change, warning, err = PlanStateChange(current, "resume", cursor.Add(time.Second))
	if err != nil || warning != nil || change.Resume == nil || !change.Resume.From.Equal(cursor) || !change.Resume.To.Equal(cursor.Add(time.Second)) {
		t.Fatalf("resume did not advance from the captured cursor: %+v %+v %v", change, warning, err)
	}
}
