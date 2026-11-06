package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPreparedSubmissionGateRetainsBindingFailureContract(t *testing.T) {
	root, binding := preparedSubmissionGateFixture(t)
	var refusal error
	if err := pueue.WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		_, refusal = supervisorcore.SubmissionGate(root)(ctx, binding)
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

// The gate consumes prepared journal DATA; no queue command, migration Start
// or provider work is needed to establish its refusal and public error contract.
func preparedSubmissionGateFixture(t *testing.T) (string, task.SupervisorRef) {
	t.Helper()
	store := newBareInspectionStore(t)
	binding := supervisorGenerationTestBinding(store.Root, "published", true)
	writeSupervisorGenerationTestPrivateGeneration(t, store, binding)
	successor := supervisorGenerationTestBinding(store.Root, "successor", true)
	record := historicalPrivateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        historicalPrivateDaemonIdentityFor(binding),
		Successor:     historicalPrivateDaemonIdentityFor(successor),
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = pueue.WithBootstrapLockContext(t.Context(), store.Root, func(context.Context) error {
		return os.WriteFile(filepath.Join(store.Root, ".supervisor", "migration.json"), data, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	if pending, readErr := pueue.PrivateSupervisorMigrationPending(store.Root); readErr != nil || !pending {
		t.Fatalf("prepared journal was not readable canonical evidence: pending=%t error=%v", pending, readErr)
	}
	if complete, readErr := pueue.PrivateSupervisorMigrationComplete(store.Root, binding); readErr != nil || complete {
		t.Fatalf("prepared generation became submission-ready: complete=%t error=%v", complete, readErr)
	}
	return store.Root, binding
}
