package app

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func requireScheduleExecutionRead(t *testing.T, response Response, code int) *ScheduleExecutionResponse {
	t.Helper()
	if code != 0 || response.Error != "" || response.ScheduleQueryError != nil {
		t.Fatalf("execution query failed: code=%d response=%+v", code, response)
	}
	if response.ScheduleExecution == nil {
		t.Fatalf("execution query omitted its detail: %+v", response)
	}
	execution := response.ScheduleExecution
	if !execution.Complete || execution.LocalEvidence == nil {
		t.Fatalf("execution query returned incomplete evidence: %+v", execution)
	}
	return execution
}

func requireScheduleExecutionFault(t *testing.T, response Response, code int, faultCode string) *ScheduleExecutionResponse {
	t.Helper()
	if code != 1 || response.ScheduleQueryError == nil || response.ScheduleQueryError.Code != faultCode {
		t.Fatalf("execution query lost its operational error: code=%d response=%+v", code, response)
	}
	if response.ScheduleExecution == nil {
		t.Fatalf("execution inspection lost its occurrence: %+v", response)
	}
	execution := response.ScheduleExecution
	if execution.Complete || execution.LocalEvidence != nil {
		t.Fatalf("inspection fault was presented as validated local evidence: %+v", execution)
	}
	return execution
}

func assertScheduleExecutionWithoutResult(t *testing.T, evidence *ScheduleExecutionEvidenceResponse) {
	t.Helper()
	if evidence.Payload != nil || evidence.OutcomeSHA256 != "" || evidence.Verdict != "" || evidence.EvidenceSHA256 != "" {
		t.Fatalf("no-result execution gained publication authority: %+v", evidence)
	}
}

func assertSchedulePublishedPayload(t *testing.T, evidence *ScheduleExecutionEvidenceResponse, winner *task.OutcomeRecord) {
	t.Helper()
	if winner == nil || evidence.Payload == nil {
		t.Fatalf("published execution omitted its result descriptor: evidence=%+v winner=%+v", evidence, winner)
	}
	if evidence.Publication != winner.Verdict || evidence.Verdict != winner.Verdict || *evidence.Payload != winner.Payload || evidence.OutcomeSHA256 == "" || evidence.EvidenceSHA256 != winner.EvidenceSHA256 {
		t.Fatalf("execution differs from its authoritative outcome: evidence=%+v winner=%+v", evidence, winner)
	}
}
