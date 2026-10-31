package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestScheduleQueryCloseFaultPreservesFactsAndSanitizesPresentation(t *testing.T) {
	const private = "private close fault /root/do-not-publish"
	closeFault := errors.New(private)
	response := newResponse("schedule")
	response.RootID, response.TaskID = strings.Repeat("a", 32), strings.Repeat("b", 32)
	response.ScheduleExecution = &ScheduleExecutionResponse{
		Occurrence: ScheduleOccurrenceResponse{TaskID: response.TaskID, ScheduleID: scheduleTestID, Reservation: "released"},
		Complete:   true, NextAction: "none", LocalEvidence: &ScheduleExecutionEvidenceResponse{
			Provenance: "validated_local", TaskPresence: "present", Publication: "committed",
			Payload: &task.PayloadDescriptor{Basename: "result.txt", Length: 42, SHA256: strings.Repeat("c", 64)},
		},
	}
	result := commandResult{response: response}
	mergeScheduleQueryClose(&result, func() error { return closeFault })
	if result.code != 1 || !errors.Is(result.err, closeFault) || result.response.RootID != response.RootID || result.response.TaskID != response.TaskID || !reflect.DeepEqual(result.response.ScheduleExecution, response.ScheduleExecution) {
		t.Fatalf("close fault erased validated facts or cause: %+v", result)
	}
	assertScheduleQueryClosePresentation(t, result, private, response.ScheduleExecution.LocalEvidence.Payload.SHA256)
}

func assertScheduleQueryClosePresentation(t *testing.T, result commandResult, private, payloadSHA256 string) {
	t.Helper()
	for _, jsonOutput := range []bool{true, false} {
		var stdout, stderr bytes.Buffer
		code := presentCommandResult(Arguments{Command: "schedule", ScheduleAction: "execution", JSON: jsonOutput}, result, &stdout, &stderr)
		if code != 1 || strings.Contains(stdout.String(), private) || strings.Contains(stderr.String(), private) {
			t.Fatalf("raw close fault leaked in presentation: json=%t code=%d stdout=%s stderr=%s", jsonOutput, code, stdout.String(), stderr.String())
		}
		if jsonOutput {
			var decoded Response
			if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.ScheduleQueryError == nil || decoded.ScheduleQueryError.Code != "query_failed" || decoded.ScheduleExecution == nil || !decoded.ScheduleExecution.Complete || decoded.ScheduleExecution.LocalEvidence.Payload.SHA256 != payloadSHA256 {
				t.Fatalf("close fault lost classified error or result descriptor: %+v", decoded)
			}
		}
	}
}

func TestScheduleQueryCloseFaultReclassifiesWholeError(t *testing.T) {
	closeFault := errors.New("private concrete close failure")
	for _, testCase := range []struct {
		name     string
		prior    error
		closeErr error
		want     string
	}{
		{name: "busy_and_close_fault", prior: task.ErrLockBusy, closeErr: closeFault, want: "query_failed"},
		{name: "busy_in_close_join", closeErr: errors.Join(task.ErrLockBusy, closeFault), want: "query_failed"},
		{name: "identity_dominates", prior: task.ErrLockBusy, closeErr: errors.Join(task.ErrIdentityMismatch, closeFault), want: "identity_mismatch"},
		{name: "evidence_dominates", prior: task.ErrLockBusy, closeErr: errors.Join(task.ErrEvidenceFault, closeFault), want: "invalid_evidence"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := newResponse("schedule")
			response.ScheduleHistory = &ScheduleHistoryResponse{ScheduleID: scheduleTestID, Entries: []ScheduleHistoryEntry{}}
			result := commandResult{response: response}
			if testCase.prior != nil {
				result = failedScheduleQuery(response, testCase.prior)
			}
			mergeScheduleQueryClose(&result, func() error { return testCase.closeErr })
			if result.code != 1 || !errors.Is(result.err, testCase.closeErr) || (testCase.prior != nil && !errors.Is(result.err, testCase.prior)) || result.response.ScheduleHistory != response.ScheduleHistory {
				t.Fatalf("close error lost query facts or cause: %+v", result)
			}
			if result.response.ScheduleQueryError == nil || result.response.ScheduleQueryError.Code != testCase.want || strings.Contains(result.response.Error, "private") || strings.Contains(result.response.Error, "retry this read") {
				t.Fatalf("joined fault retained raw or retryable busy classification: %+v", result.response)
			}
		})
	}
}

func TestScheduleQuerySuccessfulCloseKeepsBusyClassification(t *testing.T) {
	result := failedScheduleQuery(newResponse("schedule"), task.ErrLockBusy)
	before := result
	mergeScheduleQueryClose(&result, func() error { return nil })
	if result.code != before.code || !errors.Is(result.err, before.err) || !errors.Is(before.err, result.err) {
		t.Fatalf("successful close changed the query error: %+v", result)
	}
	if !reflect.DeepEqual(result.response, before.response) || result.response.ScheduleQueryError.Code != "database_busy" {
		t.Fatalf("successful close changed the actual busy refusal: %+v", result)
	}
}
