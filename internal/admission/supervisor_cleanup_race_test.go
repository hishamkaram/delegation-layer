package admission

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPrivateSuccessfulCleanupPreservesInspectionExitBeforeRemoval(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reason    inspection.ResultReason
		succeeded bool
		wantErr   error
	}{
		{name: "eligible result and successful worker", reason: inspection.ResultEligible, succeeded: true},
		{name: "eligible result but failed worker", reason: inspection.ResultEligible, wantErr: ErrInspectionWorkerUnavailable},
		{name: "failed local inspection and worker", reason: inspection.ResultUnavailable, wantErr: ErrInspectionWorkerUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertCleanupInspectionExitSaved(t, tc.reason, tc.succeeded, tc.wantErr)
		})
	}
}

func assertCleanupInspectionExitSaved(t *testing.T, reason inspection.ResultReason, wantSucceeded bool, wantErr error) {
	t.Helper()
	store, operation, job := admissionfixture.NewCleanupInspectionTestOperation(t, reason, wantSucceeded)
	if wantSucceeded {
		admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	}
	request := operation.Request()
	if _, _, proofErr := operation.ReadWorkerResultContext(t.Context()); !errors.Is(proofErr, os.ErrNotExist) {
		t.Fatalf("fixture already has independent worker observation: %v", proofErr)
	}
	if err := newInspectionTestCleanup().PrepareTerminalEvidence(t.Context(), store.Root, request.Binding.Supervisor, []pueue.Job{job}); err != nil {
		t.Fatal(err)
	}
	plan, cleanupErr := newInspectionTestCleanup().AuthorizeSuccessfulCleanup(t.Context(), store.Root, request.Binding.Supervisor, []pueue.Job{job})
	if cleanupErr != nil || len(plan.RemoveIDs) != 1 || plan.RemoveIDs[0] != job.ID || plan.Settling {
		t.Fatalf("cleanup did not authorize its completed inspection row: plan=%+v want=[%d] err=%v", plan, job.ID, cleanupErr)
	}
	result, succeeded, resultErr := operation.ReadWorkerResultContext(t.Context())
	if resultErr != nil || result.Reason != reason || succeeded != wantSucceeded {
		t.Fatalf("cleanup selected row before saving its exact worker result: result=%+v succeeded=%t err=%v", result, succeeded, resultErr)
	}
	admissionfixture.AssertCleanupInspectionTestProof(t, operation, wantSucceeded)

	// Once cleanup authorizes removal, admission must no longer need the queue
	// row. A nil supervisor makes any queue fallback invalid.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	facts, awaitErr := awaitInspectionWithSubmissionState(ctx, operation, nil, inspectionSubmissionState{})
	if !errors.Is(awaitErr, wantErr) {
		t.Fatalf("admission did not finish from the saved worker result: facts=%s err=%v wantErr=%v", facts, awaitErr, wantErr)
	}
	wantFacts := ""
	if wantSucceeded {
		wantFacts = "{\"eligible\":true}\n"
	}
	if string(facts) != wantFacts {
		t.Fatalf("saved worker result changed admission facts: got=%s want=%s", facts, wantFacts)
	}
}

func TestTerminalInspectionDecisionDoesNotOwnQueueCleanup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reason    inspection.ResultReason
		succeeded bool
		wantErr   error
	}{
		{name: "eligible success", reason: inspection.ResultEligible, succeeded: true},
		{name: "unavailable failure", reason: inspection.ResultUnavailable, wantErr: ErrInspectionWorkerUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertTerminalInspectionDecisionOnly(t, tc.reason, tc.succeeded, tc.wantErr)
		})
	}
}

func TestGuardianTerminalInspectionDecisionDoesNotOwnQueueCleanup(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	supervisor, removals, _ := newTerminalInspectionCleanupSupervisor(t, store)
	operation, job := admissionfixture.NewTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordWorkerSuccessContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	state := inspectionAwaitState{identity: operation.Request().SupervisorIdentity()}

	facts, ended, acceptErr := state.observe(t.Context(), operation, supervisor)
	if !ended || acceptErr != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("guardian-terminal inspection acceptance = facts=%s ended=%t err=%v", facts, ended, acceptErr)
	}
	if closeErr := supervisor.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if _, err := os.Lstat(removals); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guardian-terminal admission mutated its supervisor row: %v", err)
	}
}

func assertTerminalInspectionDecisionOnly(t *testing.T, reason inspection.ResultReason, succeeded bool, wantErr error) {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	supervisor, removals, statusQueries := newTerminalInspectionCleanupSupervisor(t, store)
	operation, job := admissionfixture.NewTerminalInspectionCleanupOperation(t, store, supervisor, reason, succeeded)
	if succeeded {
		admissionfixture.CompleteInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	}
	identity := operation.Request().SupervisorIdentity()

	facts, ended, acceptErr := acceptInspectionObservation(t.Context(), operation, supervisor, &identity, job)
	if !ended || !errors.Is(acceptErr, wantErr) {
		t.Fatalf("terminal inspection acceptance = facts=%s ended=%t err=%v, want err=%v", facts, ended, acceptErr, wantErr)
	}
	if succeeded && string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("successful inspection facts = %s", facts)
	}
	if !succeeded && facts != nil {
		t.Fatalf("failed inspection returned facts: %s", facts)
	}
	if closeErr := supervisor.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if _, err := os.Lstat(removals); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal inspection admission mutated its supervisor row: %v", err)
	}
	if _, err := os.Lstat(statusQueries); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal inspection admission started a cleanup observation: %v", err)
	}
	result, gotSucceeded, resultErr := operation.ReadWorkerResultContext(t.Context())
	if resultErr != nil || gotSucceeded != succeeded || result.Reason != reason {
		t.Fatalf("cleanup did not preserve terminal inspection evidence: result=%+v succeeded=%t err=%v", result, gotSucceeded, resultErr)
	}
}
