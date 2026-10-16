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

func newAppWorkspaceResource(t *testing.T) (*taskdir.Store, *workspace.Resource) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(canonicalAppTestTempDir(t), "state"))
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

func TestOrdinaryContinuationDoesNotOpenWorkspaceStore(t *testing.T) {
	response, err := prepareManagedContinuation("/nonexistent/state", Dependencies{}, task.TaskRecord{})
	if err != nil || response != nil {
		t.Fatalf("ordinary continuation touched optional workspace state: %+v, %v", response, err)
	}
}

func TestManagedContinuationRejectsReleasedWorkspaceBeforeCwdAccess(t *testing.T) {
	store, resource := newAppWorkspaceResource(t)
	if err := resource.RecordRelease(t.Context(), workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: strings.Repeat("d", 32), Discard: true}); err != nil {
		t.Fatal(err)
	}
	predecessor := task.TaskRecord{CanonicalCwd: "/removed/checkout", Workspace: &task.WorkspaceBinding{ID: resource.Request().WorkspaceID}}
	response, err := prepareManagedContinuation(store.Root, Dependencies{}, predecessor)
	if !errors.Is(err, workspace.ErrReleased) || response == nil || response.State != "cleanup_pending" {
		t.Fatalf("released workspace fell through to provider/cwd preparation: %+v, %v", response, err)
	}
}

func TestManagedContinuationDoesNotStealWorkspaceUseOwner(t *testing.T) {
	store, resource := newAppWorkspaceResource(t)
	owner, err := resource.AcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	predecessor := task.TaskRecord{Workspace: &task.WorkspaceBinding{ID: resource.Request().WorkspaceID}}
	if _, err = prepareManagedContinuation(store.Root, Dependencies{}, predecessor); !errors.Is(err, workspace.ErrBusy) {
		t.Fatalf("continuation bypassed active workspace owner: %v", err)
	}
}

func TestInitialWorkspaceAttachRetainsAdmissionDigest(t *testing.T) {
	_, resource := newAppWorkspaceResource(t)
	request := resource.Request()
	arguments := Arguments{Provider: "codex:exec", Isolation: "git-worktree", Base: request.BaseRef}
	brief := []byte("bounded task")
	initial, err := managedDispatchDigest(arguments, request.TaskID, request.SourceCwd, brief, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	arguments.Isolation, arguments.WorkspaceID = "", request.WorkspaceID
	attached, err := managedDispatchDigest(arguments, request.TaskID, "/irrelevant/checkout", brief, time.Minute, resource)
	if err != nil || initial != attached {
		t.Fatalf("known not-created attachment changed admission identity: initial=%s attached=%s err=%v", initial, attached, err)
	}
	changed, err := managedDispatchDigest(arguments, request.TaskID, "/irrelevant/checkout", []byte("changed task"), time.Minute, resource)
	if err != nil || changed == initial {
		t.Fatalf("changed provider input reused immutable admission identity: %s, %v", changed, err)
	}
}

func TestWorkspaceFailureCodesKeepProviderFailuresSeparate(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{workspace.ErrReleased, "workspace_released"},
		{workspace.ErrPending, "workspace_busy"},
		{workspace.ErrBusy, "workspace_busy"},
		{workspace.ErrIdentity, "workspace_identity_changed"},
	}
	for _, candidate := range cases {
		t.Run(candidate.code+"/"+candidate.err.Error(), func(t *testing.T) {
			failure := dispatchFailure(withDispatchStage("prepare-workspace", candidate.err), "not_created")
			if failure.Code != candidate.code || failure.Stage != "prepare-workspace" {
				t.Fatalf("incorrect failure boundary: %+v", failure)
			}
		})
	}
}

func TestWorkspaceReservationClosureBeforeTaskCreationDoesNotAwaitOutcome(t *testing.T) {
	store, resource := newAppWorkspaceResource(t)
	subject := workspace.Subject{Kind: workspace.SubjectAdmission, ID: resource.Request().TaskID, SHA256: resource.Request().RequestSHA256}
	reservation, err := resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: subject.ID,
		Workspace: &task.WorkspaceBinding{ID: resource.Request().WorkspaceID, Reservation: reservation.Sequence, RequestSHA256: subject.SHA256, BudgetNanos: int64(time.Minute)},
	}
	if err = recordManagedTaskCapture(store, request); err != nil {
		t.Fatal(err)
	}
	if err = reconcileWorkspaceUse(t.Context(), store, resource, Dependencies{}); err != nil {
		t.Fatalf("pre-task admission waited for nonexistent outcome: %v", err)
	}
	closed, err := resource.ReservationClosed(t.Context(), reservation.Sequence)
	if err != nil || closed.Disposition != "aborted-not-launched" {
		t.Fatalf("admission closure lost its not-launched disposition: %+v, %v", closed, err)
	}
	if td, openErr := store.OpenTask(subject.ID); !errors.Is(openErr, os.ErrNotExist) {
		if td != nil {
			t.Cleanup(func() {
				if closeErr := td.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			})
		}
		t.Fatalf("recovery created or could not inspect provider task: %v", openErr)
	}
	retried, err := resource.Reserve(t.Context(), subject)
	if err != nil || retried.Sequence != reservation.Sequence+1 {
		t.Fatalf("explicit retry did not receive a fresh admission reservation: %+v %v", retried, err)
	}
	previous, err := resource.ReservationClosed(t.Context(), reservation.Sequence)
	if err != nil || previous != closed {
		t.Fatalf("retry changed original admission closure: %+v %v", previous, err)
	}
}

func TestManagedWorkspaceRejectsCustomRunnerBeforeResourceCreation(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(canonicalAppTestTempDir(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	arguments := Arguments{Isolation: "git-worktree", Runner: "/unused/custom-runner"}
	managed, _, err := prepareManagedDispatch(arguments, Dependencies{}, store, strings.Repeat("a", 32), canonicalAppTestTempDir(t), []byte("bounded task"))
	if managed != nil || !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("custom runner provisioned managed state: managed=%+v err=%v", managed, err)
	}
	ids, err := workspace.ResourceIDs(t.Context(), store)
	if err != nil || len(ids) != 0 {
		t.Fatalf("rejected custom runner created workspace identities: ids=%v err=%v", ids, err)
	}
	arguments.Isolation = ""
	cwd := canonicalAppTestTempDir(t)
	managed, effective, err := prepareManagedDispatch(arguments, Dependencies{}, store, strings.Repeat("a", 32), cwd, []byte("ordinary task"))
	if err != nil || managed != nil || effective != cwd {
		t.Fatalf("ordinary custom runner behavior changed: managed=%+v cwd=%q err=%v", managed, effective, err)
	}
}
