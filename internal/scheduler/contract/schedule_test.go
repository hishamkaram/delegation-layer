package contract

import (
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInitialScheduleIsEnabledAtCreationAndOwnsItsDefinition(t *testing.T) {
	input := testDefinition()
	schedule, err := InitialSchedule(input)
	if err != nil {
		t.Fatalf("initial schedule: %v", err)
	}
	if schedule.State != StateEnabled || !schedule.Cursor.Equal(input.CreatedAt) {
		t.Fatalf("initial state %s cursor %v, want enabled at %v", schedule.State, schedule.Cursor, input.CreatedAt)
	}
	input.Brief[0] = 'X'
	input.Request.Config.ReviewTarget.Value = "changed"
	if schedule.Definition.Brief[0] != 's' || schedule.Definition.Request.Config.ReviewTarget.Value != "main" {
		t.Fatal("caller mutation reached the schedule's definition")
	}
	clone := schedule.Definition.Clone()
	clone.Brief[0] = 'Y'
	clone.Request.Config.ReviewTarget.Kind = task.ReviewTargetCommit
	if schedule.Definition.Brief[0] != 's' || schedule.Definition.Request.Config.ReviewTarget.Kind != task.ReviewTargetBase {
		t.Fatal("clone mutation reached the original definition")
	}
	invalid := testDefinition()
	invalid.Cron = ""
	if _, err := InitialSchedule(invalid); err == nil {
		t.Fatal("invalid definition produced a schedule")
	}
}

func TestValidateScheduleRequiresKnownStateAndCursorAfterCreation(t *testing.T) {
	cases := map[string]func(*Schedule){
		"unknown state":          func(s *Schedule) { s.State = "archived" },
		"empty state":            func(s *Schedule) { s.State = "" },
		"cursor before creation": func(s *Schedule) { s.Cursor = s.Definition.CreatedAt.Add(-time.Nanosecond) },
		"local cursor":           func(s *Schedule) { s.Cursor = time.Unix(5_000, 0) },
		"invalid definition":     func(s *Schedule) { s.Definition.Brief = nil },
	}
	for name, mutate := range cases {
		schedule, err := InitialSchedule(testDefinition())
		if err != nil {
			t.Fatalf("initial schedule: %v", err)
		}
		mutate(&schedule)
		if err := ValidateSchedule(schedule); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := ValidateState("archived"); !errors.Is(err, task.ErrInvalidEnum) {
		t.Fatalf("unknown state err=%v, want ErrInvalidEnum", err)
	}
}

func TestValidateSummaryVerifiesDefinitionDigestWithoutBrief(t *testing.T) {
	definition := testDefinition()
	digests, err := DefinitionDigests(definition)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	summary := ScheduleSummary{Header: definition.DefinitionHeader, Digests: digests, BriefLength: int64(len(definition.Brief)), State: StatePaused, Cursor: at(3_000)}
	if err := ValidateSummary(summary); err != nil {
		t.Fatalf("valid summary rejected: %v", err)
	}
	faults := map[string]func(*ScheduleSummary){
		"header changed":       func(s *ScheduleSummary) { s.Header.Cron = "0 0 * * *" },
		"brief digest changed": func(s *ScheduleSummary) { s.Digests.BriefSHA256 = digestOf('a') },
	}
	for name, mutate := range faults {
		changed := summary
		mutate(&changed)
		if err := ValidateSummary(changed); !errors.Is(err, task.ErrInvariantFault) {
			t.Errorf("%s: err=%v, want ErrInvariantFault", name, err)
		}
	}
	for _, length := range []int64{0, -1, task.MaxBriefSize + 1} {
		changed := summary
		changed.BriefLength = length
		if err := ValidateSummary(changed); err == nil {
			t.Errorf("brief length %d accepted", length)
		}
	}
}
