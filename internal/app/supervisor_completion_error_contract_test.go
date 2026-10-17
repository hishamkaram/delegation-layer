package app

import (
	"context"
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
)

func TestPreparedSubmissionGateRetainsBindingFailureContract(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateStashed)
	prepareBusyAdmissionMigration(t, fixture.root, fixture.predecessor, fixture.options)
	var refusal error
	if err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		_, refusal = supervisorcore.SubmissionGate(fixture.root)(ctx, fixture.predecessor)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(refusal, pueue.ErrBinding) || errors.Is(refusal, pueue.ErrPrivateSupervisorMigrationRequired) || !supervisorcore.TaskMaintenanceCompletionRefused(refusal) {
		t.Fatalf("submission refusal changed its binding-only contract: %v", refusal)
	}
	if refusal.Error() != pueue.ErrBinding.Error() || classifyCode(refusal, 2) != 1 {
		t.Errorf("submission refusal changed its message or exit code: %v", refusal)
	}
	failure := dispatchFailure(admission.WithStage("enqueue-task", refusal), "created")
	if failure.Code != "task_operation_failed" || failure.Stage != "enqueue-task" {
		t.Errorf("submission refusal changed existing task recovery guidance: %+v", failure)
	}
}
