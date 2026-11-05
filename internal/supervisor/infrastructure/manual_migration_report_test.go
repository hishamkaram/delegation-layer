package infrastructure

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func manualMigrationReportFixtures() []ManualMigrationReport {
	// These tuples are pinned to the app's initializer, not-initialized result,
	// finishSupervisorMigration branches and supervisorMigrationFailure branches.
	return []ManualMigrationReport{
		{ManualMigrationNotInitialized, ManualMigrationLegacyNotPresent, ManualMigrationActiveNone},
		{ManualMigrationMigrated, ManualMigrationLegacyNotManaged, ManualMigrationActiveUnknown},
		{ManualMigrationMigrated, ManualMigrationLegacyNotPresent, ManualMigrationActiveNone},
		{ManualMigrationMigrated, ManualMigrationLegacyNotPresent, ManualMigrationActivePresent},
		{ManualMigrationMigrated, ManualMigrationLegacyRecovered, ManualMigrationActiveNone},
		{ManualMigrationBlocked, ManualMigrationLegacyUnknown, ManualMigrationActivePresent},
		{ManualMigrationUnavailable, ManualMigrationLegacyUnknown, ManualMigrationActiveUnknown},
	}
}

func TestManualMigrationReportClosedCombinations(t *testing.T) {
	allowed := map[ManualMigrationReport]bool{}
	for _, report := range manualMigrationReportFixtures() {
		allowed[report] = true
	}
	for _, state := range []ManualMigrationState{"", "unknown", ManualMigrationNotInitialized, ManualMigrationMigrated, ManualMigrationBlocked, ManualMigrationUnavailable} {
		for _, legacy := range []ManualMigrationLegacy{"", "bad", ManualMigrationLegacyUnknown, ManualMigrationLegacyNotPresent, ManualMigrationLegacyNotManaged, ManualMigrationLegacyRecovered} {
			for _, active := range []ManualMigrationActive{"", "bad", ManualMigrationActiveUnknown, ManualMigrationActiveNone, ManualMigrationActivePresent} {
				report := ManualMigrationReport{state, legacy, active}
				if err := report.Validate(); (err == nil) != allowed[report] {
					t.Fatalf("report=%+v allowed=%t: %v", report, allowed[report], err)
				}
			}
		}
	}
}

func TestManualMigrationReportConditionTable(t *testing.T) {
	for index, report := range manualMigrationReportFixtures() {
		for _, condition := range preparationConditionFixtures() {
			wantValid := condition == PreparationFault || condition == PreparationTimeout || condition == PreparationOK && index < 5
			if err := report.ValidateCondition(condition); (err == nil) != wantValid {
				t.Fatalf("condition=%s report=%+v valid=%t: %v", condition, report, wantValid, err)
			}
		}
	}
	for _, condition := range preparationConditionFixtures() {
		if err := (ManualMigrationReport{}).ValidateCondition(condition); err != nil {
			t.Fatalf("absent report refused: %s: %v", condition, err)
		}
	}
	if err := (ManualMigrationReport{}).ValidateCondition("unknown"); !errors.Is(err, ErrIdentity) {
		t.Fatalf("unknown condition accepted: %v", err)
	}
}

func TestManualMigrationReportStrictJSON(t *testing.T) {
	for _, data := range []string{
		`null`, `{}`, `{"state":"blocked","active":"present"}`,
		`{"state":null,"legacy":"unknown","active":"present"}`,
		`{"state":"blocked","legacy":"unknown","active":null}`,
		`{"state":"blocked","legacy":"unknown","active":"present","failure":{}}`,
	} {
		var report ManualMigrationReport
		if err := task.DecodeStrict([]byte(data), &report); err == nil {
			t.Fatalf("invalid report representation decoded: %s", data)
		}
	}
	for _, report := range manualMigrationReportFixtures() {
		data, err := task.MarshalCanonical(report)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ManualMigrationReport
		if err = task.DecodeStrict(data, &decoded); err != nil || decoded != report {
			t.Fatalf("report round trip: %v", err)
		}
	}
}

func TestManualMigrationReportFailureGolden(t *testing.T) {
	for _, fixture := range []struct {
		report ManualMigrationReport
		json   string
	}{
		{ManualMigrationReport{ManualMigrationBlocked, ManualMigrationLegacyUnknown, ManualMigrationActivePresent}, "{\"state\":\"blocked\",\"legacy\":\"unknown\",\"active\":\"present\"}\n"},
		{ManualMigrationReport{ManualMigrationUnavailable, ManualMigrationLegacyUnknown, ManualMigrationActiveUnknown}, "{\"state\":\"unavailable\",\"legacy\":\"unknown\",\"active\":\"unknown\"}\n"},
	} {
		data, err := task.MarshalCanonical(fixture.report)
		if err != nil || string(data) != fixture.json {
			t.Fatalf("app failure initializer changed: got=%s want=%s err=%v", data, fixture.json, err)
		}
	}
}
