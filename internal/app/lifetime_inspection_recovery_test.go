package app

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRecoverInspectionLifetimeHistoricalWithoutRegistration(t *testing.T) {
	_, operation, _ := newCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)

	if err := recoverInspectionLifetime(t.Context(), operation); err != nil {
		t.Fatalf("historical inspection recovery = %v, want nil", err)
	}
}

func TestRecoverInspectionLifetimeCleansUnregisteredIntentWithoutTerminationEvidence(t *testing.T) {
	_, operation, _ := newCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)
	_, err := operation.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "pg:" + strings.Repeat("b", 32),
		BootID:    inspectionRecoveryBootID(t),
		Deadline:  operation.Deadline(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err = recoverInspectionLifetime(t.Context(), operation); err != nil {
		t.Fatalf("recover unregistered intent: %v", err)
	}
	if _, err = operation.ReadLifetimeRegistrationContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unregistered intent fabricated registration: %v", err)
	}
	if _, err = operation.ReadLifetimeTerminationContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unregistered intent fabricated termination: %v", err)
	}
}

func TestSuccessfulInspectionLifetimeEvidenceRejectsIncompleteManagedState(t *testing.T) {
	_, operation, job := newCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	_, err := operation.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "pg:" + strings.Repeat("d", 32),
		BootID:    inspectionRecoveryBootID(t),
		Deadline:  operation.Deadline(),
	})
	if err != nil {
		t.Fatal(err)
	}

	ready, err := successfulInspectionLifetimeEvidence(t.Context(), operation)
	if ready || err != nil {
		t.Fatalf("incomplete managed lifetime entered historical proof path: ready=%t err=%v", ready, err)
	}
	if err = validateLegacyEndedInspectionLifetime(t.Context(), operation, operation.Request().TaskID, job); !errors.Is(err, inspection.ErrGroupMigrationBusy) {
		t.Fatalf("incomplete managed lifetime migration error=%v, want busy", err)
	}
}

func TestRecoverInspectionLifetimeBeforeWorkerAcknowledgementRefusesWithoutTerminalProof(t *testing.T) {
	_, operation, _ := newCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)
	bootID := inspectionRecoveryBootID(t)
	plan := "pg:" + strings.Repeat("c", 32)
	if _, err := operation.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: plan,
		BootID:    bootID,
		Deadline:  operation.Deadline(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := operation.RecordLifetimeRegistrationContext(t.Context(), taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   plan,
		BootID:    bootID,
		Deadline:  operation.Deadline(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := recoverInspectionLifetime(t.Context(), operation); !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("recover pre-ack registration = %v, want ErrUnverifiableLaunch", err)
	}
	if _, err := operation.ReadLifetimeTerminationContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-ack recovery wrote terminal evidence: %v", err)
	}
}

func TestRecoverInspectionLifetimeChangedBootRecordsRecovery(t *testing.T) {
	_, operation, _ := newCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)
	recordInspectionLifetimeRegistration(t, operation, "different-boot", task.LifetimeAssuranceCooperative)

	if err := recoverInspectionLifetime(t.Context(), operation); err != nil {
		t.Fatalf("changed-boot inspection recovery = %v", err)
	}
	termination, err := operation.ReadLifetimeTerminationContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if termination.Cause != task.LifetimeTerminationPreviousBoot || !termination.ScopeEmpty {
		t.Fatalf("changed-boot termination = %+v", termination)
	}
}

func TestRecoverInspectionLifetimeRefusesCooperativeScopeWithoutLaunchIdentity(t *testing.T) {
	_, operation, _ := newCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)
	recordInspectionLifetimeRegistration(t, operation, inspectionRecoveryBootID(t), task.LifetimeAssuranceCooperative)

	if err := recoverInspectionLifetime(t.Context(), operation); !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("zero-ref cooperative recovery = %v, want unverifiable launch", err)
	}
	if _, err := operation.ReadLifetimeTerminationContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("zero-ref cooperative recovery wrote termination: %v", err)
	}
}

func TestRecoverInspectionLifetimeRefusesExistingTerminationWithoutWorkerWait(t *testing.T) {
	_, operation, _ := newCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)
	recordInspectionLifetimeRegistration(t, operation, inspectionRecoveryBootID(t), task.LifetimeAssuranceCooperative)
	owner, err := operation.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	termination, err := operation.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationRecovery)
	releaseErr := owner.Release()
	if err != nil {
		t.Fatal(err)
	}
	if releaseErr != nil {
		t.Fatal(releaseErr)
	}

	if err = recoverInspectionLifetime(t.Context(), operation); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("recovery with termination but no worker Wait = %v, want evidence fault", err)
	}
	current, err := operation.ReadLifetimeTerminationContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if *current != *termination {
		t.Fatalf("existing termination changed: before=%+v after=%+v", termination, current)
	}
}

func TestInspectionObservationCannotAdmitManagedSuccessWithoutLifetimeProof(t *testing.T) {
	_, operation, job := newCleanupInspectionTestOperationWithOwnership(t, inspection.ResultEligible, true, false, task.RunnerOwnershipManaged)
	recordInspectionLifetimeRegistration(t, operation, inspectionRecoveryBootID(t), task.LifetimeAssuranceCooperative)
	identity := operation.Request().SupervisorIdentity()

	facts, ended, err := acceptInspectionObservation(t.Context(), operation, nil, &identity, job)
	if facts != nil || ended || err != nil {
		t.Fatalf("ended inspection acceptance = facts=%s ended=%t err=%v", facts, ended, err)
	}
	if _, succeeded, proofErr := operation.ReadWorkerResultContext(t.Context()); !errors.Is(proofErr, os.ErrNotExist) || succeeded {
		t.Fatalf("guardian row published exact worker success: succeeded=%t err=%v", succeeded, proofErr)
	}
}

func TestFailedManagedInspectionRefusesAfterGuardianLossBeforeTermination(t *testing.T) {
	store, operation, job := newCleanupInspectionTestOperationWithOwnership(t, inspection.ResultUnavailable, false, false, task.RunnerOwnershipManaged)
	recordInspectionLifetimeRegistration(t, operation, inspectionRecoveryBootID(t), task.LifetimeAssuranceCooperative)
	identity := operation.Request().SupervisorIdentity()

	facts, ended, acceptErr := acceptInspectionObservation(t.Context(), operation, nil, &identity, job)
	if facts != nil || ended || acceptErr != nil {
		t.Fatalf("failed inspection acceptance after guardian loss = facts=%s ended=%t err=%v", facts, ended, acceptErr)
	}
	if _, _, resultErr := operation.ReadWorkerResultContext(t.Context()); !errors.Is(resultErr, os.ErrNotExist) {
		t.Fatalf("failed acceptance recorded worker exit before lifetime proof: %v", resultErr)
	}

	if err := operation.RecordWorkerExitContext(t.Context(), job.ID, false); err != nil {
		t.Fatal(err)
	}
	facts, recorded, terminal, replayErr := recordedInspectionFacts(t.Context(), operation)
	if facts != nil || recorded || terminal || replayErr != nil {
		t.Fatalf("queue failure replaced missing guardian Wait proof: facts=%s recorded=%t terminal=%t err=%v", facts, recorded, terminal, replayErr)
	}

	ids, cleanupErr := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, operation.Request().Binding.Supervisor, []pueue.Job{job})
	if cleanupErr != nil || len(ids) != 0 {
		t.Fatalf("failed inspection with unproven lifetime was authorized for cleanup: ids=%v err=%v", ids, cleanupErr)
	}
	if _, err := operation.ReadLifetimeTerminationContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guardian-loss fixture acquired false termination proof: %v", err)
	}
}

func recordInspectionLifetimeRegistration(t *testing.T, operation *inspection.Operation, bootID, assurance string) {
	t.Helper()
	owner, err := operation.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	_, err = operation.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: assurance,
		ScopePlan: "pg:" + strings.Repeat("a", 32),
		BootID:    bootID,
		Deadline:  operation.Deadline(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = operation.RecordLifetimeRegistrationContext(t.Context(), taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: assurance,
		ScopeID:   "pg:" + strings.Repeat("a", 32),
		BootID:    bootID,
		Deadline:  operation.Deadline(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = operation.RecordLifetimeWorkerContext(t.Context(), taskdir.LifetimeWorkerFacts{
		PID:          1 << 30,
		ProcessStart: "test-worker-start",
		BootID:       bootID,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func inspectionRecoveryBootID(t *testing.T) string {
	t.Helper()
	bootID, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	return bootID
}
