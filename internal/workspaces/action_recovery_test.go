package workspaces

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestWorkspaceActionSelectionFollowsUnfinishedHelperRatherThanDeadline(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: strings.Repeat("a", 32), SHA256: task.ComputeSHA256([]byte("release"))}
	old := workspacefixture.OpenRecoveryOperation(t, fixture, strings.Repeat("b", 32), workspace.OperationRemove, subject)
	t.Cleanup(func() {
		if err := old.Close(); err != nil {
			t.Error(err)
		}
	})
	workspacefixture.SeedRealOperationClosure(t, fixture.Store, old, OperationClosed)
	if err := old.RecordResult(t.Context(), workspace.OperationResult{SchemaVersion: task.SchemaVersion, OperationID: old.Request().OperationID, RequestSHA256: old.Digest(), Status: "failed", ReasonCode: "fixture_failed"}); err != nil {
		t.Fatal(err)
	}
	request := workspacefixture.RecoveryOperationRequest(t, fixture, strings.Repeat("c", 32), workspace.OperationRemove, subject)
	request.Deadline = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	current := workspacefixture.OpenSelectionOperation(t, fixture, request)
	selected, err := latestWorkspaceOperation(t.Context(), fixture.Store, fixture.Resource, workspace.OperationRemove, subject)
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
	fixture := workspacefixture.NewRecoveryFixture(t)
	subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: strings.Repeat("a", 32), SHA256: task.ComputeSHA256([]byte("release"))}
	for _, id := range []string{strings.Repeat("b", 32), strings.Repeat("c", 32)} {
		workspacefixture.OpenSelectionOperation(t, fixture, workspacefixture.RecoveryOperationRequest(t, fixture, id, workspace.OperationRemove, subject))
	}
	selected, err := latestWorkspaceOperation(t.Context(), fixture.Store, fixture.Resource, workspace.OperationRemove, subject)
	if selected != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting active helpers were silently selected: operation=%v error=%v", selected, err)
	}
}

func TestWorkspaceForgetFirstHelperCannotRenewIntentDeadline(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	intent, err := EnsureForgetIntent(t.Context(), fixture.Store, fixture.Resource, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := ForgetSubject(intent)
	if err != nil {
		t.Fatal(err)
	}
	request := workspacefixture.RecoveryOperationRequest(t, fixture, strings.Repeat("b", 32), workspace.OperationForget, subject)
	request.Deadline = intent.Deadline
	if _, err = fixture.Resource.AssociateOperation(t.Context(), ForgetPurpose(intent), request); err != nil {
		t.Fatal(err)
	}
	if err = ValidateActionRequest(t.Context(), fixture.Resource, request); err != nil {
		t.Fatal(err)
	}
	request.Deadline = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err = ValidateActionRequest(t.Context(), fixture.Resource, request); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("first helper renewed the immutable forget deadline: %v", err)
	}
}

func TestWorkspaceForgetObserverRecoversFirstHelperWithoutDeadlineRenewal(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	intent := workspace.ForgetIntent{
		SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID,
		OperationID: strings.Repeat("a", 32), BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
	}
	if err := fixture.Resource.RecordForgetIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	subject, err := ForgetSubject(intent)
	if err != nil {
		t.Fatal(err)
	}
	request := workspacefixture.RecoveryOperationRequest(t, fixture, strings.Repeat("b", 32), workspace.OperationForget, subject)
	request.Deadline = intent.Deadline
	operation, err := workspace.OpenOperation(t.Context(), fixture.Store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, err = fixture.Resource.AssociateOperation(t.Context(), ForgetPurpose(intent), request); err != nil {
		t.Fatal(err)
	}
	operationID, handled, recoverErr := ReconcileForgetOperation(t.Context(), fixture.Store, fixture.Resource, OperationOptions{}, false, 0)
	if !handled || operationID != request.OperationID || !errors.Is(recoverErr, workspace.ErrPending) {
		t.Fatalf("first-helper recovery failed: id=%s handled=%v error=%v", operationID, handled, recoverErr)
	}
	fence, err := operation.ReadLifetimeUnlaunchedContext(t.Context())
	if err != nil || fence.Deadline != intent.Deadline {
		t.Fatalf("observer renewed or launched expired first attempt: fence=%+v error=%v", fence, err)
	}
	ids, err := fixture.Resource.OperationIDs(t.Context())
	if err != nil || len(ids) != 1 || ids[0] != request.OperationID {
		t.Fatalf("observer authorized a different attempt: operations=%v error=%v", ids, err)
	}
}
