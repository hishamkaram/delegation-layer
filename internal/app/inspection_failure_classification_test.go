package app

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestInspectionFailureClassificationWorkerWithoutLocalEvidence(t *testing.T) {
	// A worker can fail before ClaimStart or native inspection. A nil operation
	// makes any attempt to treat absent local evidence as provider facts invalid.
	facts, workerErr := completedInspectionFacts(t.Context(), nil, pueue.Job{
		State: pueue.StateEnded, Succeeded: false,
	})
	assertInspectionWorkerFailureClassification(t, facts, workerErr)
}

func TestInspectionFailureClassificationRecordedWorkerFailure(t *testing.T) {
	_, operation, job := newCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)
	if recordErr := operation.RecordWorkerExitContext(t.Context(), job.ID, false); recordErr != nil {
		t.Fatal(recordErr)
	}
	facts, recorded, terminal, workerErr := recordedInspectionFacts(t.Context(), operation)
	if !recorded || !terminal {
		t.Error("persisted independent worker failure was not recognized")
	}
	assertInspectionWorkerFailureClassification(t, facts, workerErr)
}

func TestInspectionFailureClassificationActualProviderUnavailable(t *testing.T) {
	failure := dispatchFailure(withDispatchStage("inspect-provider", ErrProfileUnavailable), "not_created")
	want := DispatchFailureResponse{
		Code: "provider_unavailable", Stage: "inspect-provider", NextAction: "stop_and_report",
		Inspection: &InspectionFailureResponse{Class: "provider_unavailable", DeadlineState: "unknown"},
	}
	if !reflect.DeepEqual(failure, want) {
		t.Fatalf("genuine provider unavailability changed classification: got=%+v want=%+v", failure, want)
	}
}

func assertInspectionWorkerFailureClassification(t *testing.T, facts json.RawMessage, workerErr error) {
	t.Helper()
	if facts != nil {
		t.Errorf("failed inspection worker produced provider facts: %s", facts)
	}
	if !errors.Is(workerErr, errInspectionWorkerUnavailable) || errors.Is(workerErr, ErrProfileUnavailable) {
		t.Errorf("worker failure was interpreted as provider unavailability: err=%v want=%v", workerErr, errInspectionWorkerUnavailable)
	}
	failure := dispatchFailure(withDispatchStage("inspect-provider", workerErr), "not_created")
	want := DispatchFailureResponse{
		Code: "provider_preflight_failed", Stage: "inspect-provider", NextAction: "stop_and_report",
		Inspection: &InspectionFailureResponse{Class: "inspection_worker_failed", DeadlineState: "unknown"},
	}
	if !reflect.DeepEqual(failure, want) {
		t.Errorf("worker failure made an unsupported provider claim: got=%+v want=%+v", failure, want)
	}
}
