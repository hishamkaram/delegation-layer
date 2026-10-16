package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSeededWorkspaceArtifactVerificationStreamsAndRemovesExactFile(t *testing.T) {
	store, err := InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	operationID := strings.Repeat("a", 32)
	content := []byte(strings.Repeat("workspace artifact evidence\n", 4096))
	digest := task.ComputeSHA256(content)
	artifactPath := createWorkspaceArtifactTestFile(t, store, operationID, "stdout", content)
	if err := store.VerifyWorkspaceArtifact(t.Context(), operationID, "stdout", int64(len(content)), digest); err != nil {
		t.Fatalf("streamed verification failed: %v", err)
	}
	if err := store.VerifyWorkspaceArtifact(t.Context(), operationID, "stdout", int64(len(content)), task.ComputeSHA256([]byte("wrong"))); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("digest mismatch error=%v, want evidence fault", err)
	}
	if err := store.RemoveWorkspaceArtifact(t.Context(), operationID, "stdout", int64(len(content)), digest); err != nil {
		t.Fatalf("exact removal failed: %v", err)
	}
	if _, err := os.Stat(artifactPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact after removal stat=%v, want not exist", err)
	}
	if err := store.RemoveWorkspaceArtifact(t.Context(), operationID, "stdout", int64(len(content)), digest); err != nil {
		t.Fatalf("idempotent absent removal failed: %v", err)
	}
}

func TestSeededWorkspaceArtifactVerificationRejectsUntrustedNamesAndTypes(t *testing.T) {
	store, err := InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	operationID := strings.Repeat("b", 32)
	digest := task.ComputeSHA256([]byte("artifact"))
	for _, name := range []string{"../stdout", "..", "subdir/stdout", "\\stdout", ""} {
		if err := store.VerifyWorkspaceArtifact(t.Context(), operationID, name, 8, digest); !errors.Is(err, task.ErrEvidenceFault) {
			t.Errorf("name %q error=%v, want evidence fault", name, err)
		}
	}

	directory := filepath.Join(store.Root, "workspace-operations", operationID, "artifacts", "directory")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyWorkspaceArtifact(t.Context(), operationID, "directory", 0, task.ComputeSHA256(nil)); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("directory artifact error=%v, want evidence fault", err)
	}
}

func createWorkspaceArtifactTestFile(t *testing.T, store *Store, operationID, name string, content []byte) string {
	t.Helper()
	artifacts := filepath.Join(store.Root, "workspace-operations", operationID, "artifacts")
	if err := os.MkdirAll(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(artifacts, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
