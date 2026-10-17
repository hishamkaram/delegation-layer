package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestInspectionFailureClassificationWorkerWithoutLocalEvidence(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	t.Cleanup(func() {
		if closeErr := bound.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	client := admissionfixture.NewObservedInspectionTestClient(t, bound, logPath, inspection.AdmissionTimeout, nil)
	operation := admissionfixture.NewInspectionPollTestOperation(t, store, client)
	claim, err := operation.ClaimSubmissionContext(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = claim.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	admissionfixture.WriteInspectionPollTestStatus(t, statusPath, operation, true)
	assertNoInspectionStartOrCompletion(t, store, operation)
	facts, _, workerErr := admissioncore.RunInspection(operation, client, operation.Request().Binding.WorkerExecutable, store.Root)
	assertNoInspectionStartOrCompletion(t, store, operation)
	assertInspectionWorkerFailureClassification(t, facts, workerErr)
}

func assertNoInspectionStartOrCompletion(t *testing.T, store *taskdir.Store, operation *inspection.Operation) {
	t.Helper()
	if _, _, err := operation.ReadCompletedContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failure fixture unexpectedly has completion evidence: %v", err)
	}
	control, err := store.OpenInspectionContext(t.Context(), operation.Request().TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	var start inspection.StartRecord
	err = control.Read("start.json", &start)
	closeErr := control.Close()
	if !errors.Is(err, os.ErrNotExist) || closeErr != nil {
		t.Fatalf("failure fixture start evidence err=%v close=%v", err, closeErr)
	}
}

func TestInspectionFailureClassificationRecordedWorkerFailure(t *testing.T) {
	_, operation, job := newCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)
	if recordErr := operation.RecordWorkerExitContext(t.Context(), job.ID, false); recordErr != nil {
		t.Fatal(recordErr)
	}
	facts, recorded, terminal, workerErr := admissioncore.RecordedInspectionFacts(t.Context(), operation)
	if !recorded || !terminal {
		t.Error("persisted independent worker failure was not recognized")
	}
	assertInspectionWorkerFailureClassification(t, facts, workerErr)
}

func TestInspectionFailureClassificationActualProviderUnavailable(t *testing.T) {
	failure := dispatchFailure(admissioncore.WithStage("inspect-provider", ErrProfileUnavailable), "not_created")
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
	if !errors.Is(workerErr, admissioncore.ErrInspectionWorkerUnavailable) || errors.Is(workerErr, ErrProfileUnavailable) {
		t.Errorf("worker failure was interpreted as provider unavailability: err=%v want=%v", workerErr, admissioncore.ErrInspectionWorkerUnavailable)
	}
	failure := dispatchFailure(admissioncore.WithStage("inspect-provider", workerErr), "not_created")
	want := DispatchFailureResponse{
		Code: "provider_preflight_failed", Stage: "inspect-provider", NextAction: "stop_and_report",
		Inspection: &InspectionFailureResponse{Class: "inspection_worker_failed", DeadlineState: "unknown"},
	}
	if !reflect.DeepEqual(failure, want) {
		t.Errorf("worker failure made an unsupported provider claim: got=%+v want=%+v", failure, want)
	}
}
