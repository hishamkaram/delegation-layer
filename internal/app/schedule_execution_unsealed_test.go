package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScheduleExecutionUnsealedCaptureHasNoImmutableDigest(t *testing.T) {
	state, td, _, _ := scheduleExecutionFixture(t, false)
	if err := os.MkdirAll(filepath.Join(td.Dir, "raw"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(td.Dir, "raw", "stdout")
	writeAppTestFile(t, path, []byte("capture still being written"))
	response, code, output := runScheduleTest(t, state.Root, "execution", td.TaskID)
	if code != 0 || response.ScheduleExecution == nil || !response.ScheduleExecution.Complete {
		t.Fatalf("unsealed capture: code=%d response=%+v", code, response)
	}
	evidence := response.ScheduleExecution.LocalEvidence
	found := false
	for _, raw := range evidence.Raw {
		if raw.Path == path {
			found = raw.Available
		}
		if raw.Sealed || raw.Size != nil || raw.SHA256 != "" {
			t.Fatalf("unsealed capture gained immutable authority: %+v", raw)
		}
	}
	if !found || evidence.Liveness != "undetermined" || evidence.Publication != "unknown" || strings.Contains(output, "capture still being written") {
		t.Fatalf("unsealed facts misrepresented or inline capture leaked: %+v", evidence)
	}
}

func TestScheduleExecutionUnselectedPayloadIsNotResultAuthority(t *testing.T) {
	state, td, _, _ := scheduleExecutionFixture(t, false)
	writeAppTestFile(t, filepath.Join(td.Dir, "result.txt"), []byte("unselected payload"))
	response, code, _ := runScheduleTest(t, state.Root, "execution", td.TaskID)
	if code != 0 || response.ScheduleExecution == nil || !response.ScheduleExecution.Complete {
		t.Fatalf("unselected payload: code=%d response=%+v", code, response)
	}
	evidence := response.ScheduleExecution.LocalEvidence
	if evidence.Publication != "pending" || evidence.Payload != nil || evidence.OutcomeSHA256 != "" || evidence.Verdict != "" {
		t.Fatalf("unselected payload became success: %+v", evidence)
	}
}

func TestScheduleExecutionRefusesOrdinaryTaskWithoutOccurrence(t *testing.T) {
	state, _, _, _ := scheduleExecutionFixture(t, false)
	id := strings.Repeat("d", 32)
	ordinary, _ := newAppTaskInStore(t, state, id, false, nil)
	t.Cleanup(func() {
		if err := ordinary.Close(); err != nil {
			t.Error(err)
		}
	})
	response, code, _ := runScheduleTest(t, state.Root, "execution", id)
	if code != 1 || response.ScheduleExecution != nil || response.ScheduleQueryError == nil || response.ScheduleQueryError.Code != "not_found" {
		t.Fatalf("ordinary task was assigned schedule evidence: code=%d response=%+v", code, response)
	}
}
