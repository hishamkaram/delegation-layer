package app

import (
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestWorkbenchInfrastructureCleanupUsesInvocationDiagnostics(t *testing.T) {
	store, operation, job := newCleanupInspectionTestOperation(t, inspection.ResultUnavailable, false)
	var observed []error
	workbench := NewWorkbench(Dependencies{ReportDiagnostic: func(err error) { observed = append(observed, err) }})
	options := workbench.deps.SupervisorOptions
	// A reused numeric row ID is rejected by the actual evidence owner. The
	// callback must report that fault without publishing or authorizing removal.
	job.ID++
	supervisor := operation.Request().Binding.Supervisor
	if err := options.PrivateTerminalEvidencePreparer(t.Context(), store.Root, supervisor, []pueue.Job{job}); err != nil {
		t.Fatalf("deferred evidence fault changed callback result: %v", err)
	}
	if len(observed) != 1 || !errors.Is(observed[0], inspection.ErrGroupMigrationRequired) {
		t.Fatalf("native cleanup bypassed workbench's sink: %v", observed)
	}
	if _, _, err := operation.ReadWorkerResultContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reporting an invalid row published worker evidence: %v", err)
	}
	plan, err := options.SuccessfulCleanupAuthorizer(t.Context(), store.Root, supervisor, []pueue.Job{job})
	if err != nil || len(plan.RemoveIDs) != 0 {
		t.Fatalf("diagnostic binding changed cleanup eligibility: plan=%+v err=%v", plan, err)
	}
	if len(observed) != 1 {
		t.Fatalf("observation duplicated deferred fault: %v", observed)
	}
}
