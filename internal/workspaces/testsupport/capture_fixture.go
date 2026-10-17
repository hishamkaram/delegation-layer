package testsupport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func NewResource(t *testing.T) (*taskdir.Store, *workspace.Resource) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(supervisorfixture.CanonicalTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	resource, err := workspace.EstablishResource(t.Context(), store, workspace.ResourceRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID,
		WorkspaceID: strings.Repeat("b", 32), TaskID: strings.Repeat("c", 32),
		RequestSHA256: task.ComputeSHA256([]byte("initial admission")),
		SourceCwd:     filepath.Join(filepath.Dir(store.Root), "source"),
		Container:     workspace.ContainerPath(store.Root, store.RootID), BudgetNanos: int64(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := resource.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store, resource
}

func RecordRecoveredCaptureCandidate(t *testing.T, fixture *RecoveryFixture, operation *workspace.Operation) {
	t.Helper()
	source := RecoverySource(fixture)
	if err := fixture.Resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	candidate := workspace.CaptureCandidate{
		SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID, OperationID: operation.Request().OperationID, Subject: operation.Request().Subject,
		BaseOID: source.BaseOID, ObservedHEAD: strings.Repeat("e", 40), TreeOID: strings.Repeat("f", 40), ObjectFormat: source.ObjectFormat,
		SnapshotRef: "refs/delegate/capture-" + operation.Request().OperationID,
	}
	if err := fixture.Resource.RecordCaptureCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	if err := operation.RecordResult(t.Context(), SuccessfulCaptureResult(operation)); err != nil {
		t.Fatal(err)
	}
}

func NewFinalizeFixture(t *testing.T) (*taskdir.Store, *workspace.Resource) {
	t.Helper()
	store, resource, _, _ := NewResourceForFinalizeTests(t)
	return store, resource
}

func NewResourceForFinalizeTests(t *testing.T) (*taskdir.Store, *workspace.Resource, string, string) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(supervisorfixture.CanonicalTempDir(t), "workspace-finalize-state"))
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

func EnsureSourceDirs(root string) error {
	return os.MkdirAll(filepath.Join(root, ".git"), 0o700)
}

func SeedCanonicalReleaseCapture(t *testing.T, resource *workspace.Resource) workspace.Reservation {
	t.Helper()
	workspaceRoot := filepath.Join(t.TempDir(), "source")
	if err := EnsureSourceDirs(workspaceRoot); err != nil {
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

func SuccessfulCaptureResult(operation *workspace.Operation) workspace.OperationResult {
	return workspace.OperationResult{
		SchemaVersion: task.SchemaVersion, OperationID: operation.Request().OperationID, RequestSHA256: operation.Digest(), Status: "succeeded",
		Artifacts: []workspace.Artifact{{Name: "diff.patch", Omitted: "limit"}, {Name: "name-status.z", Omitted: "limit"}},
	}
}
