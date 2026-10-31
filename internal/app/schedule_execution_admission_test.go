package app

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestScheduleExecutionFencedNeverStartedAdmission(t *testing.T) {
	state, td, schedules, _ := scheduleExecutionFixture(t, false)
	observed, err := schedulerObservationFixture(state).Observe(td.TaskID)
	if err != nil || observed.Release == nil || observed.Release.Kind != contract.ReleaseClosed {
		t.Fatalf("runtime did not fence the prepared never-started task: %+v, %v", observed, err)
	}
	if err := schedules.RecordObservation(t.Context(), contract.OccurrenceObservation{TaskID: td.TaskID, Observation: observed.Observation}); err != nil {
		t.Fatal(err)
	}
	if err := schedules.Release(t.Context(), contract.OccurrenceRelease{TaskID: td.TaskID, Release: *observed.Release}); err != nil {
		t.Fatal(err)
	}
	response, code, _ := runScheduleTest(t, state.Root, "execution", td.TaskID)
	execution := requireScheduleExecutionRead(t, response, code)
	evidence := execution.LocalEvidence
	if evidence.Admission != task.AdmissionNotAdmitted.String() || response.Admission != task.AdmissionNotAdmitted.String() || evidence.Publication != task.PublicationUnknown.String() || response.Publication != task.PublicationUnknown.String() {
		t.Fatalf("fenced task admission or no-result publication misrepresented: %+v", response)
	}
	assertScheduleFencedExecution(t, execution, observed.Release.Ref)
}

func assertScheduleFencedExecution(t *testing.T, execution *ScheduleExecutionResponse, releaseRef string) {
	t.Helper()
	evidence := execution.LocalEvidence
	if len(evidence.Stops) != 1 || evidence.Stops[0].Cause != task.StopCauseAutomaticAbandonment || evidence.Stops[0].RequestedAt == "" {
		t.Fatalf("durable automatic refusal was lost: %+v", evidence.Stops)
	}
	assertScheduleExecutionWithoutResult(t, evidence)
	if evidence.ProviderStartedAt != "" || evidence.Closure.Closed || evidence.SupervisorReceipt {
		t.Fatalf("fenced task gained fabricated publication, admission or native closure: %+v", evidence)
	}
	if execution.Occurrence.Reservation != "released" || execution.Occurrence.Release == nil || execution.Occurrence.Release.Ref != releaseRef {
		t.Fatalf("runtime release reference was not preserved: %+v", execution.Occurrence)
	}
}

func TestScheduleExecutionSubmittedWithoutReceiptAdmission(t *testing.T) {
	state, td, _, _ := scheduleExecutionFixture(t, false)
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	permit, err := td.PrepareSubmission(meta.SupervisorConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err := permit.Release(); err != nil {
		t.Fatal(err)
	}
	response, code, _ := runScheduleTest(t, state.Root, "execution", td.TaskID)
	evidence := requireScheduleExecutionRead(t, response, code).LocalEvidence
	if evidence.Admission != task.AdmissionUnknown.String() || response.Admission != task.AdmissionUnknown.String() || evidence.SubmittedAt == "" || evidence.SupervisorReceipt || evidence.Publication != task.PublicationUnknown.String() || evidence.Payload != nil {
		t.Fatalf("submission without receipt became not-admitted or admitted: %+v", evidence)
	}
}
