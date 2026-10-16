package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestSeededValidateWorkspaceArtifactUsesForgetIntentForExactMissingBytes(t *testing.T) {
	store, _, operationID, artifact := newWorkspaceArtifactFixture(t)
	artifactPath := writeWorkspaceArtifactTestFile(t, store, operationID, artifact.Name, []byte("captured bytes"))
	if err := validateWorkspaceArtifact(t.Context(), store, operationID, artifact); err != nil {
		t.Fatalf("present artifact validation failed: %v", err)
	}
	if err := os.Remove(artifactPath); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceArtifact(t.Context(), store, operationID, artifact); err != nil {
		t.Fatalf("exact intentional absence was not accepted: %v", err)
	}
	unlisted := artifact
	unlisted.Name = "stderr"
	if err := validateWorkspaceArtifact(t.Context(), store, operationID, unlisted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unlisted missing artifact error=%v, want not-exist evidence", err)
	}
}

func newWorkspaceArtifactFixture(t *testing.T) (*taskdir.Store, *workspace.Resource, string, workspace.Artifact) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(canonicalAppTestTempDir(t), "workspace-artifact-state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeErr := store.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
	})
	workspaceID := strings.Repeat("1", 32)
	taskID := strings.Repeat("2", 32)
	request := workspace.ResourceRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, WorkspaceID: workspaceID, TaskID: taskID,
		RequestSHA256: task.ComputeSHA256([]byte("workspace request")), SourceCwd: filepath.Join(t.TempDir(), "source"),
		Container: filepath.Join(t.TempDir(), "container"), BudgetNanos: int64(time.Minute),
	}
	resource, err := workspace.EstablishResource(t.Context(), store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeErr := resource.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
	})
	operationID := strings.Repeat("3", 32)
	digest := task.ComputeSHA256([]byte("captured bytes"))
	workerDigest := task.ComputeSHA256([]byte("worker"))
	operationRequest := workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: operationID, WorkspaceID: workspaceID,
		Kind: workspace.OperationCapture, Subject: workspace.Subject{Kind: workspace.SubjectTask, ID: taskID, SHA256: request.RequestSHA256},
		Deadline: time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), WorkerExecutable: "/fixture/worker", WorkerSHA256: workerDigest,
		Supervisor: continuationSupervisorRef(store.Root, workerDigest, "unix:/fixture/pueue.sock"),
	}
	operation, err := workspace.OpenOperation(t.Context(), store, operationRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := resource.RecordOperation(t.Context(), operationRequest); err != nil {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	if err := operation.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := workspace.Artifact{Name: "stdout", Length: int64(len("captured bytes")), SHA256: digest}
	if err := resource.RecordForgetIntent(t.Context(), workspace.ForgetIntent{
		BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		SchemaVersion: task.SchemaVersion, WorkspaceID: workspaceID, OperationID: strings.Repeat("4", 32),
		Artifacts: []workspace.ForgetArtifact{{OperationID: operationID, Name: artifact.Name, Length: artifact.Length, SHA256: artifact.SHA256}},
	}); err != nil {
		t.Fatal(err)
	}
	return store, resource, operationID, artifact
}

func writeWorkspaceArtifactTestFile(t *testing.T, store *taskdir.Store, operationID, name string, content []byte) string {
	t.Helper()
	directory := filepath.Join(store.Root, "workspace-operations", operationID, "artifacts")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
