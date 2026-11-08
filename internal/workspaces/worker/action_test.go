package worker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestSeededWorkspaceForgetFailureDoesNotPublishTerminalTombstone(t *testing.T) {
	store, resource := workspacefixture.NewFinalizeFixture(t)
	workspaceRoot := filepath.Join(t.TempDir(), "source")
	if err := workspacefixture.EnsureSourceDirs(workspaceRoot); err != nil {
		t.Fatal(err)
	}
	if err := resource.RecordSource(t.Context(), workspace.Source{Root: workspaceRoot, CommonDir: filepath.Join(workspaceRoot, ".git"), GitDir: filepath.Join(workspaceRoot, ".git"), RelativeCwd: ".", BaseOID: strings.Repeat("a", 40), ObjectFormat: "sha1"}); err != nil {
		t.Fatal(err)
	}
	if err := resource.RecordRelease(t.Context(), workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: strings.Repeat("9", 32)}); err != nil {
		t.Fatal(err)
	}
	if err := resource.RecordRemoved(t.Context(), strings.Repeat("9", 32)); err != nil {
		t.Fatal(err)
	}
	operationID := strings.Repeat("a", 32)
	artifactData := []byte("actual")
	artifact := workspace.ForgetArtifact{OperationID: operationID, Name: "stdout", Length: int64(len(artifactData)), SHA256: task.ComputeSHA256([]byte("expected"))}
	directory := workspace.ScratchPath(store.Root, operationID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, artifact.Name), artifactData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resource.RecordForgetIntent(t.Context(), workspace.ForgetIntent{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: strings.Repeat("b", 32), Artifacts: []workspace.ForgetArtifact{artifact}, BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	if err := forgetManagedWorkspace(t.Context(), store, resource, workspace.Git{}); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("forget failure=%v, want evidence fault", err)
	}
	if _, err := resource.Forgotten(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("partial forget terminal record=%v, want absent", err)
	}
}

func TestWorkspaceCommandExitSeparatesOwnershipErrors(t *testing.T) {
	commandErr := exec.Command("sh", "-c", "exit 7").Run()
	if commandErr == nil {
		t.Fatal("command unexpectedly succeeded")
	}
	exitErr, resultErr := workspaceCommandExit(commandErr)
	if exitErr == nil || resultErr != nil || exitErr.ExitCode() != 7 {
		t.Fatalf("normal exit classification exit=%v err=%v", exitErr, resultErr)
	}
	combined := errors.Join(commandErr, errors.New("capture ownership failed"))
	if exitErr, resultErr = workspaceCommandExit(combined); exitErr != nil || resultErr == nil {
		t.Fatalf("combined ownership error was classified as exit: exit=%v err=%v", exitErr, resultErr)
	}
}
