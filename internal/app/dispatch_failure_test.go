package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestDispatchFailureResponseHidesSupervisorDetails(t *testing.T) {
	privateDetail := "/private/user/state/.supervisor/pueue.yml"
	err := withDispatchStage("start-supervisor", errors.Join(pueue.ErrConfiguration, errors.New(privateDetail)))
	failure := dispatchFailure(err, "not_created")
	response := newResponse("dispatch")
	response.TaskRecord = "not_created"
	response.Error = dispatchFailureMessage(failure.Code)
	response.Failure = &failure

	var output bytes.Buffer
	if writeErr := writeJSON(&output, response); writeErr != nil {
		t.Fatal(writeErr)
	}
	if strings.Contains(output.String(), privateDetail) || strings.Contains(output.String(), "pueue") {
		t.Fatalf("JSON exposed supervisor diagnostics: %s", output.String())
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TaskRecord != "not_created" || decoded.Failure == nil ||
		decoded.Failure.Code != "supervisor_setup_failed" ||
		decoded.Failure.Stage != "start-supervisor" ||
		decoded.Failure.NextAction != "stop_and_report" {
		t.Fatalf("dispatch failure recovery contract changed: %+v", decoded)
	}
}

func TestContinueResponseUsesDispatchFailureEnvelope(t *testing.T) {
	privateDetail := "/private/user/state/.supervisor/pueue.yml"
	response := newResponse("continue")
	response.TaskID = "successor-task"
	response.TaskRecord = "created"
	err := withDispatchStage("submit-task", errors.Join(pueue.ErrUnknown, pueue.ErrSubmissionUncertain, errors.New(privateDetail)))

	var output bytes.Buffer
	if writeErr := writeJSONResponse(&output, "continue", &response, err); writeErr != nil {
		t.Fatal(writeErr)
	}
	if strings.Contains(output.String(), privateDetail) || strings.Contains(output.String(), "pueue") {
		t.Fatalf("continuation JSON exposed supervisor diagnostics: %s", output.String())
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TaskID != "successor-task" || decoded.TaskRecord != "created" || decoded.Failure == nil ||
		decoded.Failure.Code != "task_submission_uncertain" ||
		decoded.Failure.Stage != "submit-task" || decoded.Failure.NextAction != "check_status" {
		t.Fatalf("continuation failure omitted safe successor recovery: %+v", decoded)
	}
}

func TestContinuePreDispatchFailureHasSpecificRecoveryEnvelope(t *testing.T) {
	result := continueTask(Arguments{Root: t.TempDir(), TaskID: strings.Repeat("a", 32)}, Dependencies{})
	if result.err == nil {
		t.Fatal("continuation unexpectedly loaded a predecessor from an empty root")
	}

	var output bytes.Buffer
	if err := writeJSONResponse(&output, "continue", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TaskRecord != "not_created" || decoded.Failure == nil ||
		decoded.Failure.Code != "continuation_unavailable" ||
		decoded.Failure.Stage != "load-predecessor" ||
		decoded.Failure.NextAction != "stop_and_report" {
		t.Fatalf("pre-dispatch continuation failure lost its recovery stage: %+v", decoded)
	}
}

func TestContinuationFailureStagesMapToActionableCodes(t *testing.T) {
	tests := []struct {
		stage string
		code  string
	}{
		{stage: "resolve-state-root", code: "state_root_unavailable"},
		{stage: "load-predecessor", code: "continuation_unavailable"},
		{stage: "resolve-continuation-provider", code: "provider_unavailable"},
		{stage: "check-continuation-capability", code: "unsupported_continuation"},
		{stage: "validate-continuation", code: "continuation_unavailable"},
		{stage: "restore-continuation-environment", code: "continuation_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.stage, func(t *testing.T) {
			failure := dispatchFailure(withDispatchStage(test.stage, errors.New("private detail")), "not_created")
			if failure.Stage != test.stage || failure.Code != test.code || failure.NextAction != "stop_and_report" {
				t.Fatalf("continuation failure stage was not classified safely: %+v", failure)
			}
			if message := dispatchFailureMessage(failure.Code); message == "Delegate could not complete dispatch." {
				t.Fatalf("continuation failure code has no safe message: %q", failure.Code)
			}
		})
	}
}

func TestDispatchTaskRecordLookupIsConservativeForExplicitIDs(t *testing.T) {
	tests := []struct {
		name       string
		taskID     string
		exists     bool
		lookupErr  error
		wantRecord string
	}{
		{name: "generated id absent", wantRecord: "not_created"},
		{name: "explicit id absent", taskID: "task-id", wantRecord: "unknown"},
		{name: "lookup failed", lookupErr: errors.New("lookup failed"), wantRecord: "unknown"},
		{name: "record exists", taskID: "task-id", exists: true, wantRecord: "created"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := dispatchTaskRecordLookup(test.taskID, test.exists, test.lookupErr); got != test.wantRecord {
				t.Fatalf("task record state=%q, want %q", got, test.wantRecord)
			}
		})
	}
}

func TestDispatchFailureNextActionTracksTaskRecord(t *testing.T) {
	uncertainErr := errors.Join(pueue.ErrUnknown, pueue.ErrSubmissionUncertain, errors.New("submission uncertain"))
	uncertain := dispatchFailure(withDispatchStage("submit-task", uncertainErr), "created")
	if uncertain.Code != "task_submission_uncertain" || uncertain.NextAction != "check_status" {
		t.Fatalf("created task did not direct the caller to status: %+v", uncertain)
	}

	unknown := dispatchFailure(withDispatchStage("create-task-record", errors.New("record write failed")), "unknown")
	if unknown.NextAction != "stop_and_report" {
		t.Fatalf("uncertain task record invited an unsafe retry: %+v", unknown)
	}
	unknownInvalid := dispatchFailure(withDispatchStage("validate-request", errors.New("bad request")), "unknown")
	if unknownInvalid.NextAction != "stop_and_report" {
		t.Fatalf("unknown task identity invited request correction: %+v", unknownInvalid)
	}

	invalid := dispatchFailure(withDispatchStage("validate-request", errors.New("bad request")), "not_created")
	if invalid.Code != "invalid_request" || invalid.NextAction != "correct_request" {
		t.Fatalf("invalid request did not direct safe correction: %+v", invalid)
	}

	unsupported := dispatchFailure(withDispatchStage("prepare-provider", commonprovider.ErrUnsupportedOption), "not_created")
	if unsupported.Code != "unsupported_option" || unsupported.NextAction != "correct_request" {
		t.Fatalf("unsupported request option did not direct safe correction: %+v", unsupported)
	}

	conflict := dispatchFailure(withDispatchStage("existing-task", task.ErrRequestConflict), "created")
	if conflict.Code != "task_request_conflict" || conflict.NextAction != "check_status" {
		t.Fatalf("existing task conflict did not require status first: %+v", conflict)
	}
}

func TestDispatchFailureCodeCoversPreparationStages(t *testing.T) {
	provider := dispatchFailure(withDispatchStage("prepare-provider", errors.New("adapter preparation failed")), "not_created")
	if provider.Code != "provider_preparation_failed" || provider.NextAction != "stop_and_report" {
		t.Fatalf("provider preparation failure was not classified: %+v", provider)
	}
	for _, stage := range []string{"validate-profile", "record-runtime-capability"} {
		failure := dispatchFailure(withDispatchStage(stage, errors.New("profile contract failed")), "not_created")
		if failure.Code != "provider_preparation_failed" || failure.NextAction != "stop_and_report" {
			t.Fatalf("%s failure was not classified as provider preparation: %+v", stage, failure)
		}
	}

	supervisor := dispatchFailure(withDispatchStage("prepare-supervisor-options", pueue.ErrConfiguration), "not_created")
	if supervisor.Code != "supervisor_setup_failed" || supervisor.NextAction != "stop_and_report" {
		t.Fatalf("supervisor option failure was not classified: %+v", supervisor)
	}

	predecessor := dispatchFailure(withDispatchStage("resolve-predecessor", errors.New("predecessor could not be resolved")), "not_created")
	if predecessor.Code != "continuation_unavailable" || predecessor.NextAction != "stop_and_report" {
		t.Fatalf("predecessor resolution failure was not classified: %+v", predecessor)
	}
}

func TestExistingSubmissionFailureKeepsUncertainSubmissionCode(t *testing.T) {
	result := markDispatchSubmissionFailure(commandResult{
		response: newResponse("dispatch"),
		err:      errors.Join(pueue.ErrUnknown, pueue.ErrSubmissionUncertain, errors.New("submission outcome unavailable")),
	})
	failure := dispatchFailure(result.err, "created")
	if failure.Code != "task_submission_uncertain" ||
		failure.Stage != "submit-task" || failure.NextAction != "check_status" {
		t.Fatalf("submission failure was not classified consistently: %+v", failure)
	}
}

func TestDispatchSubmissionFailureDistinguishesPreparationAndEvidence(t *testing.T) {
	preparation := markDispatchSubmissionFailure(commandResult{
		response: newResponse("dispatch"),
		err:      errors.New("runner validation failed"),
	})
	preparationFailure := dispatchFailure(preparation.err, "created")
	if preparationFailure.Code != "task_submission_preparation_failed" ||
		preparationFailure.Stage != "prepare-task-submission" || preparationFailure.NextAction != "check_status" {
		t.Fatalf("pre-submit failure was reported as uncertain or unrecoverable: %+v", preparationFailure)
	}

	evidenceResponse := newResponse("dispatch")
	evidenceResponse.Admission = task.AdmissionAdmitted.String()
	evidence := markDispatchSubmissionFailure(commandResult{
		response: evidenceResponse,
		err:      errors.New("receipt persistence failed"),
	})
	evidenceFailure := dispatchFailure(evidence.err, "created")
	if evidenceFailure.Code != "task_operation_failed" ||
		evidenceFailure.Stage != "record-submission-evidence" || evidenceFailure.NextAction != "check_status" {
		t.Fatalf("post-submit evidence failure was misclassified: %+v", evidenceFailure)
	}
}
