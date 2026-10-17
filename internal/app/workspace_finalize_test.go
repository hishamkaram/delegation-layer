package app

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestSeededRecordManagedTaskCapturePreservesWorkspaceBudget(t *testing.T) {
	store, resource := workspacefixture.NewFinalizeFixture(t)
	taskID := resource.Request().TaskID
	requestSHA := task.ComputeSHA256([]byte("admission request"))
	reservation, err := resource.Reserve(t.Context(), workspace.Subject{Kind: workspace.SubjectAdmission, ID: taskID, SHA256: requestSHA})
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: resource.Request().RootID, TaskID: taskID, Workspace: &task.WorkspaceBinding{
		ID: resource.Request().WorkspaceID, Reservation: reservation.Sequence, RequestSHA256: requestSHA, BudgetNanos: int64(31 * time.Second),
	}}
	err = workspaces.RecordTaskCapture(store, request)
	if err != nil {
		t.Fatal(err)
	}
	obligation, err := resource.CaptureObligation(t.Context(), reservation.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if obligation.BudgetNanos != request.Workspace.BudgetNanos {
		t.Fatalf("capture budget=%d, want %d", obligation.BudgetNanos, request.Workspace.BudgetNanos)
	}
}

func TestSeededDescribeClosedWorkspaceReservationUsesLatestIdentity(t *testing.T) {
	_, resource := workspacefixture.NewFinalizeFixture(t)
	reservation, err := resource.Reserve(t.Context(), workspace.Subject{Kind: workspace.SubjectTask, ID: strings.Repeat("7", 32), SHA256: task.ComputeSHA256([]byte("use"))})
	if err != nil {
		t.Fatal(err)
	}
	captureID := strings.Repeat("8", 32)
	receipt := workspace.CaptureReceipt{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: captureID,
		Subject: reservation.Subject, Status: "unavailable", TerminationSHA256: task.ComputeSHA256([]byte("termination")), WorkerWaitSHA256: task.ComputeSHA256([]byte("wait")),
	}
	if err := resource.RecordCaptureReceipt(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if err := resource.CloseReservation(t.Context(), reservation, "captured", captureID); err != nil {
		t.Fatal(err)
	}
	response := WorkspaceResponse{}
	if err := workspaces.DescribeClosedReservation(t.Context(), resource, &response); err != nil {
		t.Fatal(err)
	}
	if response.Capture == nil || response.Capture.OperationID != captureID {
		t.Fatalf("closed capture=%+v, want receipt %s", response.Capture, captureID)
	}
}

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
	writeWorkspaceArtifactTestFile(t, store, operationID, artifact.Name, artifactData)
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

func TestSeededWorkspaceObservationAndForgetIntentSerialize(t *testing.T) {
	_, resource := workspacefixture.NewFinalizeFixture(t)
	intent := workspace.ForgetIntent{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: strings.Repeat("c", 32), BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
	results := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, err := workspaces.Describe(t.Context(), resource)
		results <- err
	}()
	go func() {
		defer wait.Done()
		results <- resource.RecordForgetIntent(t.Context(), intent)
	}()
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent workspace transition error: %v", err)
		}
	}
	if _, err := resource.ForgetIntent(t.Context()); err != nil {
		t.Fatal(err)
	}
	description, err := workspaces.Describe(t.Context(), resource)
	if err != nil {
		t.Fatal(err)
	}
	if description.State != "forgetting" || description.EvidenceState != "forgetting" {
		t.Fatalf("forget intent description=%+v, want forgetting state", description)
	}
	if forgottenErr := resource.RecordForgotten(t.Context(), intent.OperationID); forgottenErr != nil {
		t.Fatal(forgottenErr)
	}
	description, err = workspaces.Describe(t.Context(), resource)
	if err != nil {
		t.Fatal(err)
	}
	if description.State != "forgotten" || description.EvidenceState != "forgotten" {
		t.Fatalf("forgotten description=%+v, want forgotten state", description)
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
