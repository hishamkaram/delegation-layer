package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestScheduleExecutionRejectedFailureIsQueryData(t *testing.T) {
	state, td, _, _ := scheduleExecutionFixture(t, false)
	sealScheduleFailedExecution(t, td)
	winner, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef())
	if cleanupErr != nil || collectErr != nil || winner == nil || winner.Verdict != task.VerdictRejected {
		t.Fatalf("rejected fixture: winner=%+v cleanup=%v collect=%v", winner, cleanupErr, collectErr)
	}
	response, code, output := runScheduleTest(t, state.Root, "execution", td.TaskID)
	evidence := requireScheduleExecutionRead(t, response, code).LocalEvidence
	assertSchedulePublishedPayload(t, evidence, winner)
	if evidence.Payload.Basename != "publish.reject" || evidence.ProviderExitCode == nil || *evidence.ProviderExitCode != 13 {
		t.Fatalf("failure facts lost: %+v", evidence)
	}
	for _, private := range []string{"private launch error marker", "raw provider output must not be copied", "private captured stderr"} {
		if strings.Contains(output, private) {
			t.Errorf("query leaked unbounded provider text: %q", private)
		}
	}
}

func sealScheduleFailedExecution(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err := td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err := td.WriteRawFiles("raw provider output must not be copied into a control response", "private captured stderr"); err != nil {
		t.Fatal(err)
	}
	if _, err := td.Seal(task.InvocationStarted, 13, "private launch error marker", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	if err := permit.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleExecutionProjectsStopFactsWithoutMessages(t *testing.T) {
	observed := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	id := strings.Repeat("e", 32)
	projection, err := scheduleExecutionEvidence(taskdir.TaskEvidence{
		Exists: true,
		Inspection: &taskdir.TaskInspection{Publication: task.PublicationRejected, Outcome: &task.OutcomeRecord{
			Verdict: task.VerdictRejected, Diagnostics: []task.OutputDiagnostic{{Code: "provider_exit_nonzero", Message: "private interpretation diagnostic"}},
		}},
		Seal: &task.ProviderExitRecord{ClosedAt: observed, InvocationState: task.InvocationStartFailed, ExitCode: -1, Error: "private provider launch error"},
		Stops: []taskdir.StopRecord{{
			Request:     &task.StopRequestRecord{RequestID: id, Cause: "user", RequestedAt: observed},
			Reply:       &task.StopReplyRecord{Acknowledged: true, RepliedAt: observed, Message: "private supervisor diagnostic"},
			Observation: &task.StopObservedRecord{State: "Done", Terminated: true, ObservedAt: observed},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Stops) != 1 || projection.Stops[0].RequestID != id || projection.Stops[0].Acknowledged == nil || !*projection.Stops[0].Acknowledged || projection.Stops[0].ObservedAt != observed || projection.Liveness != "undetermined" || projection.Closure.Closed {
		t.Fatalf("stop acknowledgment fabricated native closure or lost facts: %+v", projection)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("private")) {
		t.Fatalf("unrestricted messages leaked: %s", data)
	}
	if len(projection.DiagnosticCodes) != 1 || projection.DiagnosticCodes[0] != "provider_exit_nonzero" {
		t.Fatalf("classified diagnostic lost: %+v", projection)
	}
}
