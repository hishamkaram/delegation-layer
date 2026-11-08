package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func scheduleExecutionFixture(t *testing.T, sealed bool) (*taskdir.Store, *taskdir.TaskDir, contract.Store, contract.Occurrence) {
	t.Helper()
	state, td, request := newAppTestTask(t, sealed)
	t.Cleanup(func() { closeAppTestTask(t, state, td) })
	store, err := scheduler.OpenOrInitialize(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	created := time.Now().UTC().Add(-time.Hour)
	definition := contract.Definition{DefinitionHeader: contract.DefinitionHeader{
		ScheduleID: scheduleTestID, CreatedAt: created, Cron: "* * * * *", Timezone: "UTC",
		Request: contract.Request{Auto: true, CanonicalCwd: request.CanonicalCwd},
	}, Brief: []byte("brief")}
	if err = store.CreateSchedule(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	due := created.Add(time.Minute)
	if _, err = store.ClaimAndArm(t.Context(), contract.Claim{ScheduleID: definition.ScheduleID, ExpectedCursor: created, Due: due, TaskID: td.TaskID, ArmedAt: due}); err != nil {
		t.Fatal(err)
	}
	occurrence, err := store.GetOccurrence(t.Context(), td.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	return state, td, store, occurrence
}

func TestScheduleQueriesArePassiveWhileSchedulerOwnerHeld(t *testing.T) {
	state, td, store, before := scheduleExecutionFixture(t, false)
	lease, err := scheduler.AcquireOwnerLease(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	probe := filepath.Join(state.Root, ".probe")
	if removeErr := os.Remove(probe); removeErr != nil {
		t.Fatal(removeErr)
	}
	marker := []byte("passive CLI must not attempt its normal filesystem probe")
	writeAppTestFile(t, probe, marker)
	history, code, _ := runScheduleTest(t, state.Root, "history", scheduleTestID)
	if code != 0 || history.ScheduleHistory == nil || len(history.ScheduleHistory.Entries) != 1 {
		t.Fatalf("passive history failed under owner/probe barrier: code=%d %+v", code, history)
	}
	detail, code, _ := runScheduleTest(t, state.Root, "execution", td.TaskID)
	execution := requireScheduleExecutionRead(t, detail, code)
	if execution.LocalEvidence.TaskPresence != "present" {
		t.Fatalf("passive detail failed under owner/probe barrier: code=%d %+v", code, detail)
	}
	after, err := store.GetOccurrence(t.Context(), td.TaskID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("read mutated scheduler facts: before=%+v after=%+v err=%v", before, after, err)
	}
	contents, err := os.ReadFile(probe)
	if err != nil || !bytes.Equal(contents, marker) {
		t.Fatalf("passive read changed probe sentinel: %q, %v", contents, err)
	}
	for _, name := range []string{"submit.json", "provider.start", "provider.started.json", "provider.exit", "outcome.json"} {
		if _, err := os.Lstat(filepath.Join(td.Dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("passive read created %s or failed to inspect: %v", name, err)
		}
	}
}

func TestScheduleExecutionShowsSealedEvidenceWithoutPublishing(t *testing.T) {
	state, td, store, occurrence := scheduleExecutionFixture(t, true)
	cached := contract.Observation{ObservedAt: occurrence.ArmedAt, Admission: task.AdmissionAdmitted, Liveness: task.LivenessRunning, Publication: task.PublicationPending}
	if err := store.RecordObservation(t.Context(), contract.OccurrenceObservation{TaskID: td.TaskID, Observation: cached}); err != nil {
		t.Fatal(err)
	}
	response, code, _ := runScheduleTest(t, state.Root, "execution", td.TaskID)
	execution := requireScheduleExecutionRead(t, response, code)
	if execution.NextAction != "collect_task" {
		t.Fatalf("pending inspection: code=%d response=%+v", code, response)
	}
	assertSchedulePendingSeal(t, execution.LocalEvidence)
	if response.ScheduleExecution.Occurrence.Observation.Liveness != "running" {
		t.Fatal("cached liveness was replaced by a different local fact")
	}
	if _, err := os.Lstat(filepath.Join(td.Dir, "outcome.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("passive read published outcome: %v", err)
	}
	assertExecutionHasNoInventedPublicationTime(t, response)
}

func assertSchedulePendingSeal(t *testing.T, evidence *ScheduleExecutionEvidenceResponse) {
	t.Helper()
	if evidence.Provenance != "validated_local" || evidence.Publication != "pending" || evidence.Liveness != "undetermined" || evidence.ProviderStartedAt == "" || evidence.ProviderClosedAt == "" || evidence.ProviderExitCode == nil || *evidence.ProviderExitCode != 0 {
		t.Fatalf("sealed evidence misrepresented: %+v", evidence)
	}
	assertScheduleExecutionWithoutResult(t, evidence)
	if len(evidence.Raw) != 2 || !evidence.Raw[0].Sealed || !evidence.Raw[1].Sealed || evidence.Raw[0].Size == nil || evidence.Raw[0].SHA256 == "" {
		t.Fatalf("sealed descriptors missing: %+v", evidence.Raw)
	}
}

func assertExecutionHasNoInventedPublicationTime(t *testing.T, response Response) {
	t.Helper()
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Execution struct {
			Evidence struct {
				PublishedAt *time.Time `json:"published_at"`
			} `json:"local_evidence"`
		} `json:"schedule_execution"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Execution.Evidence.PublishedAt != nil {
		t.Fatal("publication timestamp was invented from an observation or file time")
	}
}

func TestScheduleExecutionCurrentPublicationDoesNotRefreshHistory(t *testing.T) {
	state, td, store, occurrence := scheduleExecutionFixture(t, true)
	observation := contract.Observation{ObservedAt: occurrence.ArmedAt, Admission: task.AdmissionAdmitted, Liveness: task.LivenessEnded, Publication: task.PublicationPending}
	if err := store.RecordObservation(t.Context(), contract.OccurrenceObservation{TaskID: td.TaskID, Observation: observation}); err != nil {
		t.Fatal(err)
	}
	release := contract.Release{ReleasedAt: occurrence.ArmedAt.Add(time.Second), Kind: contract.ReleaseClosed, Ref: strings.Repeat("d", 64)}
	if err := store.Release(t.Context(), contract.OccurrenceRelease{TaskID: td.TaskID, Release: release}); err != nil {
		t.Fatal(err)
	}
	winner, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef())
	if cleanupErr != nil || collectErr != nil {
		t.Fatalf("explicit fixture collection: cleanup=%v collect=%v", cleanupErr, collectErr)
	}
	response, code, _ := runScheduleTest(t, state.Root, "execution", td.TaskID)
	detail := requireScheduleExecutionRead(t, response, code)
	assertSchedulePublishedPayload(t, detail.LocalEvidence, winner)
	if detail.LocalEvidence.Publication != "committed" || detail.NextAction != "none" || detail.LocalEvidence.Closure.Closed {
		t.Fatalf("publication/release fabricated native closure or lost result: %+v", detail)
	}
	history, code, _ := runScheduleTest(t, state.Root, "history", scheduleTestID)
	if code != 0 || history.ScheduleHistory.Entries[0].Occurrence.Observation.Publication != "pending" || detail.Occurrence.Observation.Publication != "pending" {
		t.Fatalf("passive detail overwrote cached observation: %+v", history)
	}
	after, err := store.GetOccurrence(t.Context(), td.TaskID)
	if err != nil || after.Release == nil || *after.Release != release || after.Observation.Publication != task.PublicationPending {
		t.Fatalf("passive read mutated stored history: %+v err=%v", after, err)
	}
}

func TestScheduleExecutionWholeAbsenceIsDataAndPartialIsFault(t *testing.T) {
	root, store, definition := scheduleFixture(t)
	id := strings.Repeat("b", 32)
	due := definition.CreatedAt.Add(time.Minute)
	if _, err := store.ClaimAndArm(t.Context(), contract.Claim{ScheduleID: scheduleTestID, ExpectedCursor: definition.CreatedAt, Due: due, TaskID: id, ArmedAt: due}); err != nil {
		t.Fatal(err)
	}
	response, code, _ := runScheduleTest(t, root, "execution", id)
	execution := requireScheduleExecutionRead(t, response, code)
	if execution.LocalEvidence.TaskPresence != "absent" || execution.LocalEvidence.Publication != "unknown" || execution.Occurrence.Reservation != "active" {
		t.Fatalf("whole absence: code=%d response=%+v", code, response)
	}
	if err := store.Release(t.Context(), contract.OccurrenceRelease{TaskID: id, Release: contract.Release{ReleasedAt: due, Kind: contract.ReleaseNotCreated, Ref: "task_absent"}}); err != nil {
		t.Fatal(err)
	}
	response, code, _ = runScheduleTest(t, root, "execution", id)
	execution = requireScheduleExecutionRead(t, response, code)
	if execution.Occurrence.Release == nil || execution.Occurrence.Release.Kind != contract.ReleaseNotCreated || execution.LocalEvidence.Payload != nil {
		t.Fatalf("never-created result: code=%d response=%+v", code, response)
	}
	if err := os.MkdirAll(filepath.Join(root, "tasks", id), 0o700); err != nil {
		t.Fatal(err)
	}
	response, code, _ = runScheduleTest(t, root, "execution", id)
	execution = requireScheduleExecutionFault(t, response, code, "invalid_evidence")
	if execution.Occurrence.TaskID != id {
		t.Fatalf("partial task lost facts or became absence: code=%d response=%+v", code, response)
	}
}
