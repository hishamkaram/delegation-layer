package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

type appTaskUse struct {
	td          *taskdir.TaskDir
	reservation workspace.Reservation
	subject     workspace.Subject
}

func seedAppTaskUse(t *testing.T, store *taskdir.Store, template *taskdir.TaskDir, resource *workspace.Resource, taskID string) appTaskUse {
	t.Helper()
	request, metadata, err := template.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	digest := task.ComputeSHA256([]byte("admission " + taskID))
	reservation, err := resource.Reserve(t.Context(), workspace.Subject{Kind: workspace.SubjectAdmission, ID: taskID, SHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	clonedRequest, clonedMetadata := *request, *metadata
	clonedRequest.TaskID, clonedMetadata.TaskID = taskID, taskID
	clonedRequest.Workspace = &task.WorkspaceBinding{ID: resource.Request().WorkspaceID, Reservation: reservation.Sequence, RequestSHA256: digest, BudgetNanos: int64(time.Minute)}
	subject, err := workspacescore.TaskSubject(clonedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordCaptureObligationWithBudget(t.Context(), reservation, subject, int64(time.Minute)); err != nil {
		t.Fatal(err)
	}
	td, err := store.CreateTask(taskID, &clonedRequest, []byte("brief"), &clonedMetadata)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return appTaskUse{td: td, reservation: reservation, subject: subject}
}

func sealAppTaskUse(t *testing.T, resource *workspace.Resource, use appTaskUse, captureID string) {
	t.Helper()
	candidate := workspace.CaptureCandidate{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: captureID, Subject: use.subject,
		BaseOID: strings.Repeat("a", 40), ObservedHEAD: strings.Repeat("b", 40), TreeOID: strings.Repeat("c", 40),
		SnapshotRef: "refs/delegate/capture-" + captureID, ObjectFormat: "sha1",
	}
	if err := resource.RecordCaptureCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	receipt := workspace.CaptureReceipt{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: captureID, Subject: use.subject,
		Status: "captured", Candidate: &candidate, TerminationSHA256: task.ComputeSHA256([]byte("termination")), WorkerWaitSHA256: task.ComputeSHA256([]byte("wait")),
	}
	if err := resource.RecordCaptureReceipt(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if err := resource.CloseReservation(t.Context(), use.reservation, "captured", captureID); err != nil {
		t.Fatal(err)
	}
}

func TestManagedTaskStatusProjectsItsOwnCaptureAcrossSuccessorUse(t *testing.T) {
	store, template, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, template)
	resource, err := workspace.EstablishResource(t.Context(), store, workspace.ResourceRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, WorkspaceID: strings.Repeat("e", 32), TaskID: strings.Repeat("1", 32),
		RequestSHA256: task.ComputeSHA256([]byte("admission")), SourceCwd: filepath.Join(t.TempDir(), "source"),
		Container: workspace.ContainerPath(store.Root, store.RootID), BudgetNanos: int64(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := resource.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = resource.RecordSource(t.Context(), workspace.Source{Root: "/fixture/source", CommonDir: "/fixture/source/.git", GitDir: "/fixture/source/.git", RelativeCwd: ".", BaseOID: strings.Repeat("a", 40), ObjectFormat: "sha1"}); err != nil {
		t.Fatal(err)
	}
	first := seedAppTaskUse(t, store, template, resource, strings.Repeat("1", 32))
	firstCapture := strings.Repeat("3", 32)
	sealAppTaskUse(t, resource, first, firstCapture)
	second := seedAppTaskUse(t, store, template, resource, strings.Repeat("2", 32))
	owner, err := resource.AcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	busy := commandResult{response: newResponse("status")}
	fillManagedTaskWorkspace(store, first.td, &busy)
	if busy.response.Workspace == nil || busy.response.Workspace.Capture == nil || busy.response.Workspace.Capture.OperationID != firstCapture || busy.response.Workspace.State != "busy" {
		t.Fatalf("first task status while a successor is admitted = %+v", busy.response.Workspace)
	}
	sealAppTaskUse(t, resource, second, strings.Repeat("4", 32))
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
	later := commandResult{response: newResponse("collect")}
	fillManagedTaskWorkspace(store, first.td, &later)
	if later.response.Workspace == nil || later.response.Workspace.Capture == nil || later.response.Workspace.Capture.OperationID != firstCapture {
		t.Fatalf("first task collect adopted a successor capture: %+v", later.response.Workspace)
	}
}
