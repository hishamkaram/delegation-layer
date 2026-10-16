package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestSeededWorkspaceReleaseCaptureFailureRetainsUsableResource(t *testing.T) {
	store, resource := newWorkspaceFinalizeFixture(t)
	actionID := strings.Repeat("5", 32)
	subject, err := resource.RecordReleaseAction(t.Context(), workspace.ReleaseAction{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, ID: actionID,
		BudgetNanos: int64(17 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = completeWorkspaceReleaseCapture(t.Context(), store, resource, Dependencies{}, workspace.CaptureReceipt{Subject: subject, Status: "unavailable"})
	if !errors.Is(err, errWorkspaceCaptureUnavailable) {
		t.Fatalf("capture failure error=%v, want capture-unavailable", err)
	}
	if _, err := resource.Release(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("release record after failed capture=%v, want absent", err)
	}
	if _, err := resource.Removed(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("removed record after failed capture=%v, want absent", err)
	}
}

func TestSeededWorkspaceReleaseRecoveryPublishesRecordAfterCanonicalCapture(t *testing.T) {
	store, resource := newWorkspaceFinalizeFixture(t)
	reservation := seedCanonicalReleaseCapture(t, resource)
	reconcileErr := reconcileClosedReleaseCapture(t.Context(), store, resource, Dependencies{}, reservation.Sequence)
	if _, err := resource.Release(t.Context()); err != nil {
		t.Fatalf("release record after recovery error=%v reconcile=%v", err, reconcileErr)
	}
	if _, err := resource.Removed(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("removed record after capture-to-release recovery=%v, want absent", err)
	}
}

func TestSeededWorkspaceRemovalUsesImmutableReleaseActionBudget(t *testing.T) {
	_, resource := newWorkspaceFinalizeFixture(t)
	actionID := strings.Repeat("6", 32)
	if _, err := resource.RecordReleaseAction(t.Context(), workspace.ReleaseAction{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, ID: actionID,
		Discard: true, BudgetNanos: int64(23 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	release := workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: actionID, Discard: true}
	if err := resource.RecordRelease(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	budget, err := workspaceReleaseBudget(t.Context(), resource, release)
	if err != nil || budget != 23*time.Second {
		t.Fatalf("removal budget=%v err=%v, want 23s", budget, err)
	}
	release.Discard = false
	if _, err := workspaceReleaseBudget(t.Context(), resource, release); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("release discard mismatch error=%v, want identity mismatch", err)
	}
}

func TestSeededRecordManagedTaskCapturePreservesWorkspaceBudget(t *testing.T) {
	store, resource := newWorkspaceFinalizeFixture(t)
	taskID := resource.Request().TaskID
	requestSHA := task.ComputeSHA256([]byte("admission request"))
	reservation, err := resource.Reserve(t.Context(), workspace.Subject{Kind: workspace.SubjectAdmission, ID: taskID, SHA256: requestSHA})
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: resource.Request().RootID, TaskID: taskID, Workspace: &task.WorkspaceBinding{
		ID: resource.Request().WorkspaceID, Reservation: reservation.Sequence, RequestSHA256: requestSHA, BudgetNanos: int64(31 * time.Second),
	}}
	err = recordManagedTaskCapture(store, request)
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
	_, resource := newWorkspaceFinalizeFixture(t)
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
	if err := describeClosedWorkspaceReservation(t.Context(), resource, &response); err != nil {
		t.Fatal(err)
	}
	if response.Capture == nil || response.Capture.OperationID != captureID {
		t.Fatalf("closed capture=%+v, want receipt %s", response.Capture, captureID)
	}
}

func TestSeededWorkspaceForgetFailureDoesNotPublishTerminalTombstone(t *testing.T) {
	store, resource := newWorkspaceFinalizeFixture(t)
	workspaceRoot := filepath.Join(t.TempDir(), "source")
	if err := ensureWorkspaceSourceDirs(workspaceRoot); err != nil {
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
	_, resource := newWorkspaceFinalizeFixture(t)
	intent := workspace.ForgetIntent{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: strings.Repeat("c", 32), BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
	results := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, err := describeWorkspace(t.Context(), resource)
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
	description, err := describeWorkspace(t.Context(), resource)
	if err != nil {
		t.Fatal(err)
	}
	if description.State != "forgetting" || description.EvidenceState != "forgetting" {
		t.Fatalf("forget intent description=%+v, want forgetting state", description)
	}
	if forgottenErr := resource.RecordForgotten(t.Context(), intent.OperationID); forgottenErr != nil {
		t.Fatal(forgottenErr)
	}
	description, err = describeWorkspace(t.Context(), resource)
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

func newWorkspaceFinalizeFixture(t *testing.T) (*taskdir.Store, *workspace.Resource) {
	t.Helper()
	store, resource, _, _ := newWorkspaceResourceForFinalizeTests(t)
	return store, resource
}

func newWorkspaceResourceForFinalizeTests(t *testing.T) (*taskdir.Store, *workspace.Resource, string, string) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(canonicalAppTestTempDir(t), "workspace-finalize-state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeErr := store.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
	})
	workspaceID, taskID := strings.Repeat("d", 32), strings.Repeat("e", 32)
	request := workspace.ResourceRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, WorkspaceID: workspaceID, TaskID: taskID,
		RequestSHA256: task.ComputeSHA256([]byte("finalize request")), SourceCwd: filepath.Join(t.TempDir(), "source"),
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
	return store, resource, workspaceID, taskID
}

func ensureWorkspaceSourceDirs(root string) error {
	return os.MkdirAll(filepath.Join(root, ".git"), 0o700)
}

func seedCanonicalReleaseCapture(t *testing.T, resource *workspace.Resource) workspace.Reservation {
	t.Helper()
	workspaceRoot := filepath.Join(t.TempDir(), "source")
	if err := ensureWorkspaceSourceDirs(workspaceRoot); err != nil {
		t.Fatal(err)
	}
	source := workspace.Source{Root: workspaceRoot, CommonDir: filepath.Join(workspaceRoot, ".git"), GitDir: filepath.Join(workspaceRoot, ".git"), RelativeCwd: ".", BaseOID: strings.Repeat("a", 40), ObjectFormat: "sha1"}
	if err := resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	actionID := strings.Repeat("f", 32)
	subject, err := resource.RecordReleaseAction(t.Context(), workspace.ReleaseAction{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, ID: actionID, BudgetNanos: int64(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if err := resource.RecordReleaseCapture(t.Context(), reservation, subject); err != nil {
		t.Fatal(err)
	}
	captureID := strings.Repeat("a", 32)
	candidate := workspace.CaptureCandidate{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: captureID, Subject: subject,
		BaseOID: source.BaseOID, ObservedHEAD: strings.Repeat("b", 40), TreeOID: strings.Repeat("c", 40),
		SnapshotRef: "refs/delegate/capture-" + captureID, ObjectFormat: source.ObjectFormat,
	}
	if err := resource.RecordCaptureCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	if err := resource.RecordCaptureReceipt(t.Context(), workspace.CaptureReceipt{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: captureID, Subject: subject,
		Status: "captured", Candidate: &candidate, TerminationSHA256: task.ComputeSHA256([]byte("termination")), WorkerWaitSHA256: task.ComputeSHA256([]byte("wait")),
	}); err != nil {
		t.Fatal(err)
	}
	if err := resource.CloseReservation(t.Context(), reservation, "captured", captureID); err != nil {
		t.Fatal(err)
	}
	return reservation
}
