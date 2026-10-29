package contract

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// goldenDefinitionSHA256 pins the canonical digest record of testDefinition. It
// was computed independently from the hand-written canonical JSON record.
const goldenDefinitionSHA256 = "b867de55e07d8b98e0cd1f8daf741be70e3d8f1d232b0ff77c59fb5c4758252b"

func TestValidateDefinitionBoundsEveryImmutableField(t *testing.T) {
	cases := map[string]func(*Definition){
		"empty brief":                func(d *Definition) { d.Brief = nil },
		"brief over limit":           func(d *Definition) { d.Brief = make([]byte, task.MaxBriefSize+1) },
		"uppercase schedule id":      func(d *Definition) { d.ScheduleID = strings.ToUpper(testScheduleID) },
		"short schedule id":          func(d *Definition) { d.ScheduleID = testScheduleID[:31] },
		"local created_at":           func(d *Definition) { d.CreatedAt = time.Unix(1_000, 0) },
		"zero created_at":            func(d *Definition) { d.CreatedAt = time.Time{} },
		"empty cron":                 func(d *Definition) { d.Cron = "" },
		"long cron":                  func(d *Definition) { d.Cron = strings.Repeat("*", MaxOpaqueTextBytes+1) },
		"control in cron":            func(d *Definition) { d.Cron = "0 9 * * *\n" },
		"invalid utf8 timezone":      func(d *Definition) { d.Timezone = "Europe/\xff" },
		"control in timezone":        func(d *Definition) { d.Timezone = "UTC\u0085" },
		"invalid request selection":  func(d *Definition) { d.Request.Auto = true },
		"interactive request config": func(d *Definition) { d.Request.Config.Interactive = true },
	}
	for name, mutate := range cases {
		definition := testDefinition()
		mutate(&definition)
		if err := ValidateDefinition(definition); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	bounded := testDefinition()
	bounded.Brief = make([]byte, task.MaxBriefSize)
	bounded.Cron = strings.Repeat("é", MaxOpaqueTextBytes/2)
	if err := ValidateDefinition(bounded); err != nil {
		t.Fatalf("definition at every bound rejected: %v", err)
	}
	if err := ValidateDefinition(testDefinition()); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
}

func TestDefinitionDigestBindsEveryImmutableField(t *testing.T) {
	base, err := DefinitionDigests(testDefinition())
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if base.BriefSHA256 != task.ComputeSHA256(testDefinition().Brief) {
		t.Fatalf("brief digest %s is not the brief's SHA-256", base.BriefSHA256)
	}
	if base.DefinitionSHA256 != goldenDefinitionSHA256 {
		t.Fatalf("definition digest changed to %s; stored digests would no longer verify", base.DefinitionSHA256)
	}
	changes := map[string]func(*Definition){
		"schedule id": func(d *Definition) { d.ScheduleID = strings.Repeat("a", 32) },
		"created_at":  func(d *Definition) { d.CreatedAt = d.CreatedAt.Add(time.Nanosecond) },
		"cron":        func(d *Definition) { d.Cron = "0 10 * * 1-5" },
		"timezone":    func(d *Definition) { d.Timezone = "UTC" },
		"provider":    func(d *Definition) { d.Request.Provider = "claude:print" },
		"cwd":         func(d *Definition) { d.Request.CanonicalCwd = "/work/other" },
		"model":       func(d *Definition) { d.Request.Config.Model = "other" },
		"review target value": func(d *Definition) {
			d.Request.Config.ReviewTarget = &task.ReviewTarget{Kind: task.ReviewTargetBase, Value: "dev"}
		},
		"brief": func(d *Definition) { d.Brief = []byte("summarize closed issues") },
	}
	for name, mutate := range changes {
		definition := testDefinition()
		mutate(&definition)
		changed, err := DefinitionDigests(definition)
		if err != nil || changed.DefinitionSHA256 == base.DefinitionSHA256 {
			t.Errorf("%s: digest %s err=%v did not change", name, changed.DefinitionSHA256, err)
		}
	}
}

func TestDefinitionDigestExcludesMutableStateAndCursor(t *testing.T) {
	schedule, err := InitialSchedule(testDefinition())
	if err != nil {
		t.Fatalf("initial schedule: %v", err)
	}
	before, err := DefinitionDigests(schedule.Definition)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	schedule.State = StateDeleted
	schedule.Cursor = schedule.Cursor.Add(time.Hour)
	if validationErr := ValidateSchedule(schedule); validationErr != nil {
		t.Fatalf("advanced schedule rejected: %v", validationErr)
	}
	after, err := DefinitionDigests(schedule.Definition)
	if err != nil || after != before {
		t.Fatalf("state or cursor changed digests: %+v -> %+v err=%v", before, after, err)
	}
}

func TestVerifyDefinitionDigestsReportsTamperingAsInvariantFault(t *testing.T) {
	definition := testDefinition()
	stored, err := DefinitionDigests(definition)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if err := VerifyDefinitionDigests(definition, stored); err != nil {
		t.Fatalf("matching digests rejected: %v", err)
	}
	tampered := definition.Clone()
	tampered.Brief[0] ^= 1
	wrongDefinition := Digests{BriefSHA256: stored.BriefSHA256, DefinitionSHA256: digestOf('0')}
	for name, check := range map[string]error{
		"tampered brief":            VerifyDefinitionDigests(tampered, stored),
		"wrong definition digest":   VerifyDefinitionDigests(definition, wrongDefinition),
		"wrong brief digest":        VerifyDefinitionDigests(definition, Digests{BriefSHA256: digestOf('0'), DefinitionSHA256: stored.DefinitionSHA256}),
		"empty stored brief digest": VerifyDefinitionDigests(definition, Digests{DefinitionSHA256: stored.DefinitionSHA256}),
	} {
		if !errors.Is(check, task.ErrInvariantFault) {
			t.Errorf("%s: err=%v, want ErrInvariantFault", name, check)
		}
	}
	if _, err := HeaderSHA256(definition.DefinitionHeader, "ABC"); err == nil {
		t.Fatal("malformed brief digest accepted")
	}
}
