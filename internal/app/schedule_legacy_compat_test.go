package app

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func materializeLegacySchedule(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "legacy")
	source := os.DirFS(filepath.Join("testdata", "scheduler-v1", "state"))
	err := fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func legacyScheduleSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[relative] = task.ComputeSHA256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestScheduleLegacyV1HistoryAndEvidenceArePassive(t *testing.T) {
	root := materializeLegacySchedule(t)
	before := legacyScheduleSnapshot(t, root)
	history, code, _ := runScheduleTest(t, root, "history", strings.Repeat("a", 32))
	if code != 0 || history.ScheduleHistory == nil || len(history.ScheduleHistory.Entries) != 2 {
		t.Fatalf("pre-change history unreadable: code=%d response=%+v", code, history)
	}
	for index, fixture := range []struct {
		id          string
		publication task.Publication
	}{
		{strings.Repeat("d", 32), task.PublicationCommitted},
		{strings.Repeat("c", 32), task.PublicationPending},
	} {
		assertLegacyReleasedOccurrence(t, history.ScheduleHistory.Entries[index].Occurrence, fixture.id)
		assertLegacyExecutionEvidence(t, root, fixture.id, fixture.publication.String())
	}
	if !reflect.DeepEqual(before, legacyScheduleSnapshot(t, root)) {
		t.Fatal("passive queries modified pre-change persisted bytes")
	}
}

func assertLegacyReleasedOccurrence(t *testing.T, row *ScheduleOccurrenceResponse, id string) {
	t.Helper()
	if row == nil || row.TaskID != id || row.Reservation != "released" || row.Release == nil {
		t.Fatalf("pre-change occurrence changed: %+v", row)
	}
}

func assertLegacyExecutionEvidence(t *testing.T, root, id, publication string) {
	t.Helper()
	execution, code, _ := runScheduleTest(t, root, "execution", id)
	if code != 0 || execution.ScheduleExecution == nil || !execution.ScheduleExecution.Complete {
		t.Fatalf("pre-change evidence unreadable: code=%d response=%+v", code, execution)
	}
	facts := execution.ScheduleExecution.LocalEvidence
	if facts == nil || facts.Publication != publication || !facts.Closure.Closed || len(facts.Raw) != 2 {
		t.Fatalf("pre-change facts changed: %+v", facts)
	}
}

func TestScheduleLegacyV1CollectRepairsSameReleasedOccurrence(t *testing.T) {
	root := materializeLegacySchedule(t)
	id := strings.Repeat("c", 32)
	before, code, _ := runScheduleTest(t, root, "execution", id)
	if code != 0 || before.ScheduleExecution.LocalEvidence.Publication != task.PublicationPending.String() {
		t.Fatalf("pre-change pending fixture invalid: %+v", before)
	}
	startPath := filepath.Join(root, "tasks", id, "provider.start")
	start, err := os.ReadFile(startPath)
	if err != nil {
		t.Fatal(err)
	}
	result := collect(Arguments{Root: root, TaskID: id}, storeDependencies{})
	if result.err != nil || result.code != 0 || result.response.Publication != task.PublicationCommitted.String() {
		t.Fatalf("old sealed task could not publish: %+v", result)
	}
	after, code, _ := runScheduleTest(t, root, "execution", id)
	if code != 0 || after.ScheduleExecution.LocalEvidence.Publication != task.PublicationCommitted.String() || !reflect.DeepEqual(before.ScheduleExecution.Occurrence, after.ScheduleExecution.Occurrence) {
		t.Fatalf("recovery changed old reservation/cache: before=%+v after=%+v", before, after)
	}
	payload, err := os.ReadFile(filepath.Join(root, "tasks", id, "result.txt"))
	if err != nil || string(payload) != "answer\n" {
		t.Fatalf("old fixture answer changed: %q %v", payload, err)
	}
	currentStart, err := os.ReadFile(startPath)
	if err != nil || string(start) != string(currentStart) {
		t.Fatalf("collection changed the original start: %v", err)
	}
}
