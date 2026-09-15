package pueue

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

const statusTestTime = "2026-09-13T20:00:00.123456789Z"

func TestParseActualQueuedStatusFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/status-4.0.4-queued.json")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := ParseStatus(data, SupportedVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != 0 || jobs[0].State != StateQueued || jobs[0].Label == nil || *jobs[0].Label != "fixture:queued" {
		t.Fatalf("actual serialized response was misread: %+v", jobs)
	}
}

func statusTestPayload(t *testing.T, jobs map[string]any, groups map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"tasks": jobs, "groups": groups})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func statusTestJob(id int64, label any, state map[string]any) map[string]any {
	return map[string]any{
		"id":               id,
		"created_at":       statusTestTime,
		"original_command": "runner --root /tmp/task",
		"command":          "runner --root /tmp/task",
		"path":             "/tmp",
		"envs":             map[string]string{},
		"group":            "default",
		"dependencies":     []int64{},
		"priority":         int32(0),
		"label":            label,
		"status":           state,
	}
}

func TestParseStatusAcceptsEverySupportedLifecycleState(t *testing.T) {
	states := map[string]map[string]any{
		"Queued":  {"Queued": map[string]any{"enqueued_at": statusTestTime}},
		"Running": {"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}},
		"Paused":  {"Paused": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}},
		"Stashed": {"Stashed": map[string]any{"enqueue_at": nil}},
		"Done":    {"Done": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime, "end": statusTestTime, "result": "Success"}},
		"Locked":  {"Locked": map[string]any{"previous_status": map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}}}},
	}
	for name, state := range states {
		t.Run(name, func(t *testing.T) {
			jobs := map[string]any{"7": statusTestJob(7, "delegate:"+strings.Repeat("a", 32)+":"+strings.Repeat("b", 32), state)}
			jobsData := statusTestPayload(t, jobs, map[string]any{})
			parsed, err := ParseStatus(jobsData, SupportedVersion)
			if err != nil {
				t.Fatal(err)
			}
			if len(parsed) != 1 || parsed[0].ID != 7 {
				t.Fatalf("unexpected rows: %+v", parsed)
			}
			if name == "Locked" && parsed[0].State != StateUnknown {
				t.Fatalf("locked state should remain conservative: %+v", parsed[0])
			}
		})
	}
}

func TestParseStatusAcceptsTaskResultVariants(t *testing.T) {
	results := map[string]any{
		"success":         "Success",
		"escaped_success": json.RawMessage(`"Succ\u0065ss"`),
		"killed":          "Killed",
		"errored":         "Errored",
		"dependency":      "DependencyFailed",
		"failed":          map[string]any{"Failed": int32(9)},
		"failed_to_spawn": map[string]any{"FailedToSpawn": "no executable"},
	}
	for name, result := range results {
		t.Run(name, func(t *testing.T) {
			state := map[string]any{"Done": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime, "end": statusTestTime, "result": result}}
			data := statusTestPayload(t, map[string]any{"7": statusTestJob(7, "label", state)}, map[string]any{})
			jobs, err := ParseStatus(data, SupportedVersion)
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 1 || jobs[0].Succeeded != (name == "success" || name == "escaped_success") {
				t.Fatalf("worker success was not distinguished from termination: %+v", jobs)
			}
		})
	}
}

func TestQueueSnapshotPreservesInspectionSchedulingFacts(t *testing.T) {
	job := statusTestJob(7, "inspection", map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}})
	job["group"] = "inspection-root"
	data := statusTestPayload(t, map[string]any{"7": job}, map[string]any{
		"inspection-root": map[string]any{"status": "Running", "parallel_tasks": 1},
		"unrelated":       map[string]any{"status": "Paused", "parallel_tasks": 3},
	})
	snapshot, err := ParseQueueSnapshot(data, SupportedVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].Group != "inspection-root" || snapshot.Jobs[0].Succeeded {
		t.Fatalf("queued inspection facts changed: %+v", snapshot.Jobs)
	}
	if snapshot.Groups["inspection-root"] != (Group{Status: "Running", ParallelTasks: 1}) || snapshot.Groups["unrelated"] != (Group{Status: "Paused", ParallelTasks: 3}) {
		t.Fatalf("queue settings were lost: %+v", snapshot.Groups)
	}
}

func TestParseStatusRejectsMalformedRowsAndBindingVersion(t *testing.T) {
	validJob := statusTestJob(7, "label", map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}})
	valid := statusTestPayload(t, map[string]any{"7": validJob}, map[string]any{})
	cases := map[string][]byte{
		"wrong version":       valid,
		"missing tasks":       []byte(`{"groups":{}}`),
		"missing row field":   statusTestPayload(t, map[string]any{"7": map[string]any{"id": 7}}, map[string]any{}),
		"null row":            statusTestPayload(t, map[string]any{"7": nil}, map[string]any{}),
		"negative map id":     statusTestPayload(t, map[string]any{"-1": validJob}, map[string]any{}),
		"overflow map id":     statusTestPayload(t, map[string]any{"9223372036854775808": validJob}, map[string]any{}),
		"leading zero map id": statusTestPayload(t, map[string]any{"07": validJob}, map[string]any{}),
		"negative row id":     statusTestPayload(t, map[string]any{"7": statusTestJob(-1, "label", map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}})}, map[string]any{}),
		"wrong row id":        statusTestPayload(t, map[string]any{"7": statusTestJob(8, "label", map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}})}, map[string]any{}),
		"unknown state":       statusTestPayload(t, map[string]any{"7": statusTestJob(7, "label", map[string]any{"Unknown": nil})}, map[string]any{}),
		"null state":          statusTestPayload(t, map[string]any{"7": statusTestJob(7, "label", nil)}, map[string]any{}),
		"bad timestamp":       statusTestPayload(t, map[string]any{"7": statusTestJob(7, "label", map[string]any{"Queued": map[string]any{"enqueued_at": "later"}})}, map[string]any{}),
		"bad group":           statusTestPayload(t, map[string]any{"7": validJob}, map[string]any{"default": map[string]any{"status": "Unknown", "parallel_tasks": uint64(1)}}),
		"priority overflow":   statusPayloadWithPriorityOverflow(t),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			version := SupportedVersion
			if name == "wrong version" {
				version = "4.0.3"
			}
			if _, err := ParseStatus(data, version); err == nil {
				t.Fatal("malformed status accepted")
			} else if name != "wrong version" && !errors.Is(err, ErrUnknown) {
				t.Fatalf("malformed status did not remain unknown: %v", err)
			}
		})
	}
}

func statusPayloadWithPriorityOverflow(t *testing.T) []byte {
	t.Helper()
	job := statusTestJob(7, "label", map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}})
	job["priority"] = int64(2147483648)
	return statusTestPayload(t, map[string]any{"7": job}, map[string]any{})
}

func TestParseStatusRejectsDuplicateKeysAndRows(t *testing.T) {
	duplicateField := `{"tasks":{"7":{"id":7,"created_at":"` + statusTestTime + `","original_command":"x","command":"x","path":"/tmp","envs":{},"group":"default","dependencies":[],"priority":0,"label":"label","status":{"Queued":{"enqueued_at":"` + statusTestTime + `"}},"label":"other"}},"groups":{}}`
	if _, err := ParseStatus([]byte(duplicateField), SupportedVersion); err == nil {
		t.Fatal("duplicate field accepted")
	}
	duplicateCase := `{"tasks":{"7":{"id":7,"created_at":"` + statusTestTime + `","original_command":"x","command":"x","path":"/tmp","envs":{},"group":"default","dependencies":[],"priority":0,"label":"label","LABEL":"other","status":{"Queued":{"enqueued_at":"` + statusTestTime + `"}}}},"groups":{}}`
	if _, err := ParseStatus([]byte(duplicateCase), SupportedVersion); err == nil {
		t.Fatal("case-folded duplicate field accepted")
	}
	duplicateRows := `{"tasks":{"7":{"id":7,"created_at":"` + statusTestTime + `","original_command":"x","command":"x","path":"/tmp","envs":{},"group":"default","dependencies":[],"priority":0,"label":"label","status":{"Queued":{"enqueued_at":"` + statusTestTime + `"}}},"8":{"id":7,"created_at":"` + statusTestTime + `","original_command":"x","command":"x","path":"/tmp","envs":{},"group":"default","dependencies":[],"priority":0,"label":"label","status":{"Queued":{"enqueued_at":"` + statusTestTime + `"}}}},"groups":{}}`
	if _, err := ParseStatus([]byte(duplicateRows), SupportedVersion); err == nil {
		t.Fatal("reused task ID accepted")
	}
}

func TestMatchingObservationRequiresOneExactLabelAndTarget(t *testing.T) {
	identity := Identity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64)}
	label := identity.Label()
	if observation, err := matchingObservation([]Job{{ID: 7, Label: &label, State: StateRunning}}, identity, validBindingForTest()); err != nil || !observation.Matched || observation.State != StateRunning {
		t.Fatalf("positive match failed: %+v %v", observation, err)
	}
	wrong := "delegate:" + strings.Repeat("a", 32) + ":" + strings.Repeat("e", 32)
	if _, err := matchingObservation([]Job{{ID: 7, Label: &wrong, State: StateRunning}}, identity, validBindingForTest()); !errors.Is(err, ErrUnknown) {
		t.Fatalf("wrong label was accepted: %v", err)
	}
	target := int64(8)
	if _, err := matchingObservation([]Job{{ID: 7, Label: &label, State: StateRunning}}, Identity{RootID: identity.RootID, TaskID: identity.TaskID, SpecSHA256: identity.SpecSHA256, MetaSHA256: identity.MetaSHA256, NumericTaskID: &target}, validBindingForTest()); !errors.Is(err, ErrUnknown) {
		t.Fatalf("wrong numeric target was accepted: %v", err)
	}
}
