package admission

import (
	"context"
	"errors"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestPrivateInspectionPreparationWorksUnderExistingGenerationOwner(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		name := "shared-maintenance"
		if exclusive {
			name = "exclusive-migration"
		}
		t.Run(name, func(t *testing.T) { verifyPrivatePreparationGenerationOwner(t, exclusive) })
	}
}

func verifyPrivatePreparationGenerationOwner(t *testing.T, exclusive bool) {
	t.Helper()
	store, operation, job := admissionfixture.NewCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)
	if err := pueue.WithBootstrapLock(t.Context(), store.Root, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	prepare := func(owned context.Context) error {
		return newInspectionTestCleanup().PrepareTerminalEvidence(owned, store.Root, operation.Request().Binding.Supervisor, []pueue.Job{job})
	}
	var err error
	if exclusive {
		err = pueue.WithBootstrapLockContext(ctx, store.Root, prepare)
	} else {
		err = pueue.WithSupervisorLeaseContext(ctx, store.Root, prepare)
	}
	if err != nil {
		t.Fatalf("preparation re-entered its generation owner: %v", err)
	}
	plan, err := newInspectionTestCleanup().AuthorizeSuccessfulCleanup(ctx, store.Root, operation.Request().Binding.Supervisor, []pueue.Job{job})
	if err != nil || len(plan.RemoveIDs) != 1 || plan.RemoveIDs[0] != job.ID {
		t.Fatalf("prepared row did not authorize from durable evidence: %+v, %v", plan, err)
	}
	facts, awaitErr := awaitInspectionWithSubmissionState(ctx, operation, nil, inspectionSubmissionState{})
	if facts != nil || !errors.Is(awaitErr, ErrInspectionWorkerUnavailable) {
		t.Fatalf("admission lost terminal evidence after queue removal: %s, %v", facts, awaitErr)
	}
}
