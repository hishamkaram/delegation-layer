package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPrivateInspectionAuthorizationCannotPublishItsOwnExitProof(t *testing.T) {
	store, operation, job := newCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)
	plan, err := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, operation.Request().Binding.Supervisor, []pueue.Job{job})
	if err != nil || len(plan.RemoveIDs) != 0 {
		t.Fatalf("authorization accepted an unprepared inspection: plan=%+v err=%v", plan, err)
	}
	if _, _, err = operation.ReadWorkerResultContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authorization published its own worker observation: %v", err)
	}
}

func TestPrivateInspectionPreparationDoesNotWaitForLiveLifetimeOwner(t *testing.T) {
	store, operation, job := newCleanupInspectionTestOperationWithOwnership(t, inspection.ResultUnavailable, false, false, task.RunnerOwnershipManaged)
	recordInspectionLifetimeRegistration(t, operation, testsupport.InspectionRecoveryBootID(t), task.LifetimeAssuranceCooperative)
	owner, err := operation.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := owner.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = preparePrivateTerminalEvidence(ctx, store.Root, operation.Request().Binding.Supervisor, []pueue.Job{job}); err != nil {
		t.Fatalf("preparation waited on a live guardian: %v", err)
	}
	if _, err = operation.ReadLifetimeWorkerWaitContext(ctx); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live guardian received a fabricated completion: %v", err)
	}
	plan, err := authorizePrivateSuccessfulCleanup(ctx, store.Root, operation.Request().Binding.Supervisor, []pueue.Job{job})
	if err != nil || len(plan.RemoveIDs) != 0 {
		t.Fatalf("live guardian row authorized: %+v, %v", plan, err)
	}
}

func TestPrivateInspectionPreparationRejectsDuplicateAndReusedRows(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "reused-numeric-id", true: "duplicate-label"}[duplicate], func(t *testing.T) {
			store, operation, job := newCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)
			changed := job
			changed.ID++
			jobs := []pueue.Job{changed}
			if duplicate {
				jobs = append(jobs, job)
			}
			if err := preparePrivateTerminalEvidence(t.Context(), store.Root, operation.Request().Binding.Supervisor, jobs); err != nil {
				t.Fatal(err)
			}
			if _, _, err := operation.ReadWorkerResultContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("ambiguous snapshot published exit authority: %v", err)
			}
		})
	}
}
