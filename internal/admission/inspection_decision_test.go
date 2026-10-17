package admission

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

var errUnexpectedInspectionLifetimeOwner = errors.New("inspection decision acquired lifetime owner")

type rejectInspectionLifetimeOwnerOpen struct {
	taskdir.BaseFaultInjector
}

type transientInspectionOperationLock struct {
	taskdir.BaseFaultInjector
	failed bool
}

func (*transientInspectionOperationLock) OnLockFileBarrier(string) error {
	return nil
}

func (*transientInspectionOperationLock) OnLockParentDirBarrier(string) error {
	return nil
}

func (f *transientInspectionOperationLock) OnFlock(path string, _ int) error {
	if filepath.Base(path) == ".operation.lock" && !f.failed {
		f.failed = true
		return task.ErrLockBusy
	}
	return nil
}

func (rejectInspectionLifetimeOwnerOpen) OnLockFileBarrier(path string) error {
	if filepath.Base(path) == ".lifetime-owner.lock" {
		return errUnexpectedInspectionLifetimeOwner
	}
	return nil
}

func (rejectInspectionLifetimeOwnerOpen) OnLockParentDirBarrier(string) error {
	return nil
}

func (rejectInspectionLifetimeOwnerOpen) OnFlock(string, int) error {
	return nil
}

func TestInspectionAdmissionDecisionDoesNotAcquireLifetimeOwner(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, _, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperationAtWithOwnership(t, store, bound, time.Now().UTC(), task.RunnerOwnershipManaged)
	completeInspectionPollTestOperation(t, operation)
	if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordWorkerSuccessContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	owner, err := operation.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	store.SetFaultInjector(&rejectInspectionLifetimeOwnerOpen{})
	defer store.SetFaultInjector(nil)

	facts, recorded, terminal, err := RecordedInspectionFacts(t.Context(), operation)
	if err != nil || !recorded || !terminal || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("durable proof depended on lifetime recovery: facts=%s recorded=%t terminal=%t err=%v", facts, recorded, terminal, err)
	}
}

func TestManagedInspectionAdmissionRequiresLifetimeTermination(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, _, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperationAtWithOwnership(t, store, bound, time.Now().UTC(), task.RunnerOwnershipManaged)
	completeInspectionPollTestOperation(t, operation)
	if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordWorkerSuccessContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}

	facts, recorded, terminal, err := RecordedInspectionFacts(t.Context(), operation)
	if facts != nil || recorded || terminal || err != nil {
		t.Fatalf("managed worker without empty-scope proof was admitted: facts=%s recorded=%t terminal=%t err=%v", facts, recorded, terminal, err)
	}
}

func TestTimelyCompletedInspectionEvidenceRejectsMissingLifetimeRegistration(t *testing.T) {
	store, operation, _ := admissionfixture.NewCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	registrationPath := filepath.Join(store.Root, "inspections", operation.Request().TaskID, "lifetime-registration.json")
	if err := os.Remove(registrationPath); err != nil {
		t.Fatal(err)
	}

	ready, err := timelyCompletedInspectionEvidence(t.Context(), operation)
	if ready {
		t.Fatal("missing lifetime registration was treated as pending")
	}
	if !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing lifetime registration error = %v, want identity mismatch and os.ErrNotExist", err)
	}
}

func TestRecordedInspectionFactsRejectsMissingManagedLifetimeRegistration(t *testing.T) {
	store, operation, _ := admissionfixture.NewCleanupInspectionTestOperationWithOwnership(
		t, inspection.ResultEligible, true, false, task.RunnerOwnershipManaged,
	)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	registrationPath := filepath.Join(store.Root, "inspections", operation.Request().TaskID, "lifetime-registration.json")
	if err := os.Remove(registrationPath); err != nil {
		t.Fatal(err)
	}

	facts, recorded, terminal, err := RecordedInspectionFacts(t.Context(), operation)
	if facts != nil || recorded || terminal {
		t.Fatalf("missing managed lifetime registration changed the decision state: facts=%s recorded=%t terminal=%t", facts, recorded, terminal)
	}
	if !errors.Is(err, task.ErrIdentityMismatch) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing managed lifetime registration error = %v, want identity mismatch and os.ErrNotExist", err)
	}
}

func TestInspectionStatusCannotOverrideManagedGuardianProof(t *testing.T) {
	for _, tc := range []struct {
		name        string
		writeStatus func(*testing.T, string, *inspection.Operation)
	}{
		{
			name: "guardian row failed",
			writeStatus: func(t *testing.T, path string, operation *inspection.Operation) {
				writeInspectionPollTestTerminalStatus(t, path, operation, false)
			},
		},
		{
			name:        "guardian row removed",
			writeStatus: writeInspectionPollTestNoMatchStatus,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := supervisorfixture.NewInspectionStore(t)
			bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
			operation := admissionfixture.NewInspectionPollTestOperationAtWithOwnership(t, store, bound, time.Now().UTC(), task.RunnerOwnershipManaged)
			completeInspectionPollTestOperation(t, operation)
			if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
				t.Fatal(err)
			}
			admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
			statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
			tc.writeStatus(t, statusPath, operation)
			published := make(chan error, 1)
			var publishOnce sync.Once
			client := newInspectionPollTestClient(t, bound, logPath, func(event pueue.CommandEvent) {
				if event.Stage == "completed" && slices.Contains(event.Argv, "status") {
					publishOnce.Do(func() {
						published <- operation.RecordWorkerSuccessContext(t.Context(), 7)
					})
				}
			})
			identity := operation.Request().SupervisorIdentity()
			facts, ended, err := pollInspectionStatus(t.Context(), operation, client, &identity)
			if publishErr := <-published; publishErr != nil {
				t.Fatal(publishErr)
			}
			if err != nil || !ended || string(facts) != "{\"eligible\":true}\n" {
				t.Fatalf("supervisor row overrode guardian proof: facts=%s ended=%t err=%v", facts, ended, err)
			}
			assertInspectionStatusCommandCount(t, logPath, 1)
		})
	}
}

func TestInspectionGraceUsesDurableProofAfterQueueRemoval(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperationAtWithOwnership(t, store, bound, time.Now().UTC(), task.RunnerOwnershipManaged)
	completeInspectionPollTestOperation(t, operation)
	if err := operation.RecordReceiptContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordWorkerSuccessContext(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	writeInspectionPollTestNoMatchStatus(t, filepath.Join(filepath.Dir(logPath), "status.json"), operation)
	client := newInspectionPollTestClient(t, bound, logPath, nil)

	facts, err := awaitCompletedInspectionObservation(operation, client, inspection.ErrAdmissionExpired)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("grace observation ignored durable proof: facts=%s err=%v", facts, err)
	}
	assertInspectionStatusCommandCount(t, logPath, 0)
}

func TestInspectionAwaitObserverFailureCannotOverrideDurableProof(t *testing.T) {
	_, operation, job := admissionfixture.NewCleanupInspectionTestOperationWithOwnership(
		t, inspection.ResultEligible, true, false, task.RunnerOwnershipManaged,
	)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordWorkerSuccessContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	client, err := pueue.NewClient(operation.Request().Binding.Supervisor, pueue.Options{
		Environment: os.Environ(), ObservationTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()

	facts, err := awaitInspectionWithSubmissionState(
		t.Context(), operation, client, inspectionSubmissionState{},
	)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("observer setup overrode durable proof: facts=%s err=%v", facts, err)
	}
}

func TestInspectionAwaitRetriesJournalContentionWithinAdmissionDeadline(t *testing.T) {
	store, operation, job := admissionfixture.NewCleanupInspectionTestOperationWithOwnership(
		t, inspection.ResultEligible, true, false, task.RunnerOwnershipManaged,
	)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordWorkerSuccessContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	fault := &transientInspectionOperationLock{}
	store.SetFaultInjector(fault)
	defer store.SetFaultInjector(nil)

	facts, err := AwaitInspection(t.Context(), operation, nil)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("transient journal contention ended admission: facts=%s err=%v", facts, err)
	}
	if !fault.failed {
		t.Fatal("test did not inject journal lock contention")
	}
}

func TestFailedManagedWorkerProofDoesNotRunLifetimeRecovery(t *testing.T) {
	store, operation, job := admissionfixture.NewCleanupInspectionTestOperationWithOwnership(t, inspection.ResultUnavailable, false, false, task.RunnerOwnershipManaged)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordWorkerExitContext(t.Context(), job.ID, false); err != nil {
		t.Fatal(err)
	}
	owner, err := operation.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	store.SetFaultInjector(&rejectInspectionLifetimeOwnerOpen{})
	defer store.SetFaultInjector(nil)

	facts, recorded, terminal, err := RecordedInspectionFacts(t.Context(), operation)
	if facts != nil || !recorded || !terminal || !errors.Is(err, ErrInspectionWorkerUnavailable) {
		t.Fatalf("failed proof depended on lifetime recovery: facts=%s recorded=%t terminal=%t err=%v", facts, recorded, terminal, err)
	}
}
