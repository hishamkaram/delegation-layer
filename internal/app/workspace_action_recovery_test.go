package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestWorkspaceActionSelectionFollowsUnfinishedHelperRatherThanDeadline(t *testing.T) {
	fixture := newWorkspaceRecoveryFixture(t)
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: strings.Repeat("a", 32), SHA256: task.ComputeSHA256([]byte("release"))}
	old := openRecoveryOperation(t, fixture, strings.Repeat("b", 32), workspace.OperationRemove, subject)
	t.Cleanup(func() {
		if err := old.Close(); err != nil {
			t.Error(err)
		}
	})
	seedRealWorkspaceOperationClosure(t, fixture.store, old)
	if err := old.RecordResult(t.Context(), workspace.OperationResult{SchemaVersion: task.SchemaVersion, OperationID: old.Request().OperationID, RequestSHA256: old.Digest(), Status: "failed", ReasonCode: "fixture_failed"}); err != nil {
		t.Fatal(err)
	}
	request := recoveryOperationRequest(t, fixture, strings.Repeat("c", 32), workspace.OperationRemove, subject)
	request.Deadline = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	current := openWorkspaceSelectionOperation(t, fixture, request)
	selected, err := latestWorkspaceOperation(t.Context(), fixture.store, fixture.resource, workspace.OperationRemove, subject)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := selected.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if selected.Request().OperationID != current.Request().OperationID {
		t.Fatalf("closed longer-budget attempt hid the unfinished retry: selected=%s current=%s", selected.Request().OperationID, current.Request().OperationID)
	}
}

func TestWorkspaceActionSelectionRejectsMultipleUnfinishedHelpers(t *testing.T) {
	fixture := newWorkspaceRecoveryFixture(t)
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: strings.Repeat("a", 32), SHA256: task.ComputeSHA256([]byte("release"))}
	for _, id := range []string{strings.Repeat("b", 32), strings.Repeat("c", 32)} {
		openWorkspaceSelectionOperation(t, fixture, recoveryOperationRequest(t, fixture, id, workspace.OperationRemove, subject))
	}
	selected, err := latestWorkspaceOperation(t.Context(), fixture.store, fixture.resource, workspace.OperationRemove, subject)
	if selected != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting active helpers were silently selected: operation=%v error=%v", selected, err)
	}
}

func openWorkspaceSelectionOperation(t *testing.T, fixture *workspaceRecoveryFixture, request workspace.OperationRequest) *workspace.Operation {
	t.Helper()
	operation, err := workspace.OpenOperation(t.Context(), fixture.store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = fixture.resource.RecordOperation(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	return operation
}

func TestWorkspaceForgetIntentKeepsFirstBudgetAndDeadline(t *testing.T) {
	store, resource := newWorkspaceFinalizeFixture(t)
	intent, err := ensureWorkspaceForgetIntent(t.Context(), store, resource, 17*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ensureWorkspaceForgetIntent(t.Context(), store, resource, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.OperationID != intent.OperationID || replayed.BudgetNanos != int64(17*time.Second) || replayed.Deadline != intent.Deadline {
		t.Fatalf("replay renewed original forget authorization: initial=%+v replay=%+v", intent, replayed)
	}
	result := workspaceCommandActionResponse(t.Context(), store, resource, "forget", false, workspaceActionAcknowledgement{}, newResponse("workspace"), workspace.ErrPending)
	if result.code != 3 || result.response.Workspace.NextAction != "watch_workspace" || result.response.Workspace.OperationID != "" {
		t.Fatalf("helperless forget intent was treated as an unaccepted action: %+v", result)
	}
}

func TestWorkspaceForgetFirstHelperCannotRenewIntentDeadline(t *testing.T) {
	fixture := newWorkspaceRecoveryFixture(t)
	intent, err := ensureWorkspaceForgetIntent(t.Context(), fixture.store, fixture.resource, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := workspaceForgetSubject(intent)
	if err != nil {
		t.Fatal(err)
	}
	request := recoveryOperationRequest(t, fixture, strings.Repeat("b", 32), workspace.OperationForget, subject)
	request.Deadline = intent.Deadline
	if _, err = fixture.resource.AssociateOperation(t.Context(), workspaceForgetPurpose(intent), request); err != nil {
		t.Fatal(err)
	}
	if err = validateWorkspaceActionRequest(t.Context(), fixture.resource, request); err != nil {
		t.Fatal(err)
	}
	request.Deadline = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err = validateWorkspaceActionRequest(t.Context(), fixture.resource, request); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("first helper renewed the immutable forget deadline: %v", err)
	}
}

func TestWorkspaceForgetObserverRecoversFirstHelperWithoutDeadlineRenewal(t *testing.T) {
	fixture := newWorkspaceRecoveryFixture(t)
	intent := workspace.ForgetIntent{
		SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.resource.Request().WorkspaceID,
		OperationID: strings.Repeat("a", 32), BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
	}
	if err := fixture.resource.RecordForgetIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	subject, err := workspaceForgetSubject(intent)
	if err != nil {
		t.Fatal(err)
	}
	request := recoveryOperationRequest(t, fixture, strings.Repeat("b", 32), workspace.OperationForget, subject)
	request.Deadline = intent.Deadline
	operation, err := workspace.OpenOperation(t.Context(), fixture.store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, err = fixture.resource.AssociateOperation(t.Context(), workspaceForgetPurpose(intent), request); err != nil {
		t.Fatal(err)
	}
	operationID, handled, recoverErr := reconcileWorkspaceForgetOperation(t.Context(), fixture.store, fixture.resource, Dependencies{}, false, 0)
	if !handled || operationID != request.OperationID || !errors.Is(recoverErr, workspace.ErrPending) {
		t.Fatalf("first-helper recovery failed: id=%s handled=%v error=%v", operationID, handled, recoverErr)
	}
	fence, err := operation.ReadLifetimeUnlaunchedContext(t.Context())
	if err != nil || fence.Deadline != intent.Deadline {
		t.Fatalf("observer renewed or launched expired first attempt: fence=%+v error=%v", fence, err)
	}
	ids, err := fixture.resource.OperationIDs(t.Context())
	if err != nil || len(ids) != 1 || ids[0] != request.OperationID {
		t.Fatalf("observer authorized a different attempt: operations=%v error=%v", ids, err)
	}
}
