package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestScheduleQueriesDoNotInitializeMissingStores(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing_root", true: "missing_database"}[initialized], func(t *testing.T) {
			assertScheduleQueriesDoNotInitializeMissingStore(t, initialized)
		})
	}
}

func assertScheduleQueriesDoNotInitializeMissingStore(t *testing.T, initialized bool) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if initialized {
		store, err := taskdir.InitStore(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, action := range []string{"history", "execution"} {
		response, code, _ := runScheduleTest(t, root, action, scheduleTestID)
		if code != 1 || response.ScheduleQueryError == nil || response.ScheduleQueryError.Code != "not_found" || response.Error == "" {
			t.Fatalf("%s: code=%d response=%+v", action, code, response)
		}
		if response.ScheduleHistory != nil && response.ScheduleHistory.Entries != nil {
			t.Fatal("missing storage was reported as a successful empty history")
		}
	}
	if _, err := os.Lstat(filepath.Join(root, ".scheduler")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read initialized scheduler storage: %v", err)
	}
	if !initialized {
		if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read initialized missing root: %v", err)
		}
	}
}

func TestScheduleQueryMissingRowsAreNotEmptySuccess(t *testing.T) {
	root, _, _ := scheduleFixture(t)
	id := strings.Repeat("f", 32)
	for _, action := range []string{"history", "execution"} {
		response, code, _ := runScheduleTest(t, root, action, id)
		if code != 1 || response.ScheduleQueryError == nil || response.ScheduleQueryError.Code != "not_found" {
			t.Fatalf("missing %s row: code=%d response=%+v", action, code, response)
		}
		if response.ScheduleHistory != nil && response.ScheduleHistory.Entries != nil {
			t.Fatal("missing schedule was reported as an empty page")
		}
	}
}

func TestScheduleQueriesInvalidInputPrecedesOpeningRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	for _, argv := range [][]string{
		{"history", scheduleTestID, "--limit", "0"},
		{"history", scheduleTestID, "--limit", "101"},
		{"history", scheduleTestID, "--before", "2026-10-31"},
		{"execution", "invalid"},
	} {
		var stdout, stderr bytes.Buffer
		args := append([]string{"--root", root, "schedule"}, argv...)
		code := Run(append(args, "--json"), &stdout, &stderr, Dependencies{})
		if code != 2 {
			t.Errorf("argv=%v code=%d stdout=%s stderr=%s", argv, code, stdout.String(), stderr.String())
		}
	}
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("invalid query initialized root: %v", err)
	}
}

func TestScheduleExecutionRetainsOccurrenceOnIdentityFault(t *testing.T) {
	state, td, _, occurrence := scheduleExecutionFixture(t, true)
	path := filepath.Join(td.Dir, "provider.started.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var started task.ProviderStartedRecord
	if err = json.Unmarshal(contents, &started); err != nil {
		t.Fatal(err)
	}
	started.TaskID = strings.Repeat("d", 32)
	data, err := json.Marshal(started)
	if err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, path, data)
	response, code, _ := runScheduleTest(t, state.Root, "execution", td.TaskID)
	execution := requireScheduleExecutionFault(t, response, code, "identity_mismatch")
	if execution.Occurrence.TaskID != td.TaskID || !execution.Occurrence.Due.Equal(occurrence.Due) || execution.Occurrence.Observation.Provenance != "unobserved" {
		t.Fatalf("failed inspection erased known occurrence or exposed invalid evidence: %+v", execution)
	}
}

func TestScheduleExecutionResponseNeverTruncatesDescriptors(t *testing.T) {
	response := newResponse("schedule")
	payload := task.PayloadDescriptor{Basename: "result.txt", Length: 42, SHA256: strings.Repeat("a", 64)}
	raw := taskdir.LogDescriptor{Path: strings.Repeat("x", MaxJSONResponseBytes), Available: true}
	response.ScheduleExecution = &ScheduleExecutionResponse{Complete: true, NextAction: "none", LocalEvidence: &ScheduleExecutionEvidenceResponse{Payload: &payload, Raw: []taskdir.LogDescriptor{raw}}}
	var output bytes.Buffer
	if err := writeJSON(&output, response); !errors.Is(err, ErrJSONResponseTooLarge) || output.Len() != 0 {
		t.Fatalf("oversize descriptors were emitted or truncated: len=%d err=%v", output.Len(), err)
	}
	if *response.ScheduleExecution.LocalEvidence.Payload != payload || response.ScheduleExecution.LocalEvidence.Raw[0].Path != raw.Path {
		t.Fatal("presentation mutated authoritative descriptors")
	}
	if err := writeHumanResponse(&output, response); !errors.Is(err, ErrJSONResponseTooLarge) || output.Len() != 0 {
		t.Fatalf("oversize human output was emitted: len=%d err=%v", output.Len(), err)
	}
}
