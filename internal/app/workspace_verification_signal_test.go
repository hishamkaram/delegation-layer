package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// This self-terminating command tests result interpretation. Native deadline
// stopping is exercised by the guardian and native acceptance tests.
type workspaceSignalTestStopper struct{}

func (workspaceSignalTestStopper) PrepareBudget(time.Time) (execution.BudgetRequest, error) {
	return func(context.Context) error { return errors.New("unexpected signal test budget expiry") }, nil
}

func TestSignaledWorkspaceVerificationRetainsDurableDiagnostics(t *testing.T) {
	store, operation := newWorkspaceSignalTestOperation(t)
	request, deadline := operation.Request(), operation.Deadline()
	var candidate workspace.OperationResult
	workErr, stopErr := execution.RunSupervised(deadline, execution.SupervisedOptions{Stopper: workspaceSignalTestStopper{}}, func(scope execution.PreflightScope) error {
		var actionErr error
		candidate, actionErr = runWorkspaceAction(scope, store, operation)
		return actionErr
	})
	if workErr != nil || stopErr != nil {
		t.Fatalf("signal result escaped its action interpretation: %v %v", workErr, stopErr)
	}
	if candidate.Status != "failed" || candidate.ReasonCode != "verification_command_signaled" || candidate.CommandExit != nil || len(candidate.Artifacts) != 2 {
		t.Fatalf("signal discarded the observed command result: %+v", candidate)
	}
	if err := recordWorkspaceWorkerResult(t.Context(), store, operation, candidate); err != nil {
		t.Fatalf("signal diagnostics could not be committed: %v", err)
	}
	stored, err := operation.Result(t.Context())
	if err != nil || stored.ReasonCode != "verification_command_signaled" || len(stored.Artifacts) != 2 {
		t.Fatalf("stored signal verdict=%+v error=%v", stored, err)
	}
	for _, artifact := range stored.Artifacts {
		if err = validateWorkspaceArtifact(t.Context(), store, request.OperationID, artifact); err != nil {
			t.Fatal(err)
		}
		content, readErr := os.ReadFile(filepath.Join(workspace.ScratchPath(store.Root, request.OperationID), artifact.Name))
		if readErr != nil || string(content) != "signal-"+artifact.Name {
			t.Fatalf("signal lost %s content: %q %v", artifact.Name, content, readErr)
		}
	}
}

func newWorkspaceSignalTestOperation(t *testing.T) (*taskdir.Store, *workspace.Operation) {
	t.Helper()
	resource, _ := newMappedWorkspaceExecutionFixture(t)
	store, err := openStore(resource.StateRoot(), Dependencies{}.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	digest := task.ComputeSHA256([]byte("signal verification"))
	deadline := time.Now().Add(time.Minute)
	request := workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, WorkspaceID: resource.Request().WorkspaceID,
		OperationID: strings.Repeat("f", 32), Kind: workspace.OperationVerify,
		Subject:  workspace.Subject{Kind: workspace.SubjectAdmission, ID: strings.Repeat("e", 32), SHA256: digest},
		Deadline: deadline.UTC().Format(time.RFC3339Nano), WorkerExecutable: "/fixture/delegate-run", WorkerSHA256: digest,
		Supervisor: continuationSupervisorRef(store.Root, digest, "unix:/fixture/pueue.sock"),
		Arguments:  []string{"/bin/sh", "-c", "printf signal-stdout; printf signal-stderr >&2; kill -9 $$"},
	}
	operation, err := workspace.OpenOperation(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store, operation
}
