package admission

import (
	"context"
	"errors"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRunAdmissionInspectionReplaysRecordedFactsAfterDeadline(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, _, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	createdAt := time.Now().UTC().Add(-inspection.AdmissionTimeout - time.Second)
	operation := admissionfixture.NewInspectionPollTestOperationAt(t, store, bound, createdAt)
	completeInspectionPollTestOperation(t, operation)
	if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	rewriteInspectionPollTestTerminationTime(t, store, operation, createdAt.Add(4*time.Millisecond))
	if err := operation.RecordWorkerSuccessContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}

	facts, deadline, err := RunInspection(operation, nil, "", "")
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("expired replay discarded validated admission facts: facts=%s deadline=%v err=%v", facts, deadline, err)
	}
	if !operation.Expired(time.Now()) {
		t.Fatal("replay fixture was not expired")
	}
}

func TestRunAdmissionInspectionReplaysHistoricalRecordedFactsAfterDeadline(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, _, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	createdAt := time.Now().UTC().Add(-inspection.AdmissionTimeout - time.Second)
	operation := admissionfixture.NewInspectionPollTestOperationAt(t, store, bound, createdAt)
	completeInspectionPollTestOperation(t, operation)
	if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordWorkerSuccessContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}

	facts, deadline, err := RunInspection(operation, nil, "", "")
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("historical replay discarded validated admission facts: facts=%s deadline=%v err=%v", facts, deadline, err)
	}
	if !operation.Expired(time.Now()) {
		t.Fatal("historical replay fixture was not expired")
	}
}

func TestRunAdmissionInspectionInitialReadUsesOperationDeadline(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, _, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	createdAt := time.Now().UTC().Add(-inspection.AdmissionTimeout + 2*time.Second)
	operation := admissionfixture.NewInspectionPollTestOperationAt(t, store, bound, createdAt)

	control, err := store.OpenInspection(operation.Request().TaskID, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- control.WithLock(context.Background(), func(_ *taskdir.ControlTransaction) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	result := make(chan error, 1)
	go func() {
		_, _, runErr := RunInspection(operation, nil, "", "")
		result <- runErr
	}()
	select {
	case runErr := <-result:
		if !errors.Is(runErr, inspection.ErrAdmissionExpired) {
			t.Fatalf("initial evidence read ignored deadline error: %v", runErr)
		}
	case <-time.After(3 * time.Second):
		close(release)
		<-lockDone
		t.Fatal("initial evidence read outlived the operation deadline")
	}
	close(release)
	if err = <-lockDone; err != nil {
		t.Fatal(err)
	}
}

func TestValidateLegacyEndedInspectionLifetimeAcceptsHistoricalSuccess(t *testing.T) {
	_, operation, job := admissionfixture.NewCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	if err := operation.RecordWorkerSuccessContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}

	if err := ValidateInspectionLifetime(t.Context(), operation, operation.Request().TaskID, job); err != nil {
		t.Fatalf("historical ended inspection was rejected during migration: %v", err)
	}
}

func TestInspectionPollingStopsWhenSupervisorCommandIsInFlight(t *testing.T) {
	pending := &pueue.Pending{}
	if !shouldStopInspectionPolling(&pueue.InFlightError{Pending: pending}) {
		t.Fatal("in-flight supervisor command remained pollable")
	}
	if shouldStopInspectionPolling(&pueue.InFlightError{}) {
		t.Fatal("in-flight error without a retained command stopped polling")
	}
	if shouldStopInspectionPolling(pueue.ErrUnknown) {
		t.Fatal("ordinary observation uncertainty stopped polling")
	}
}
