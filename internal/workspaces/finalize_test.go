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

func TestSeededWorkspaceReleaseCaptureFailureRetainsUsableResource(t *testing.T) {
	store, resource := workspacefixture.NewFinalizeFixture(t)
	actionID := strings.Repeat("5", 32)
	subject, err := resource.RecordReleaseAction(t.Context(), workspace.ReleaseAction{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, ID: actionID,
		BudgetNanos: int64(17 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = completeWorkspaceReleaseCapture(t.Context(), store, resource, OperationOptions{}, workspace.CaptureReceipt{Subject: subject, Status: "unavailable"})
	if !errors.Is(err, ErrCaptureUnavailable) {
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
	store, resource := workspacefixture.NewFinalizeFixture(t)
	reservation := workspacefixture.SeedCanonicalReleaseCapture(t, resource)
	reconcileErr := reconcileClosedReleaseCapture(t.Context(), store, resource, OperationOptions{Runtime: unavailableOperationRuntime{}}, reservation.Sequence)
	if !errors.Is(reconcileErr, errOperationFixtureRuntimeUnavailable) {
		t.Fatalf("unexpected runtime preparation result: %v", reconcileErr)
	}
	if _, err := resource.Release(t.Context()); err != nil {
		t.Fatalf("release record after recovery error=%v reconcile=%v", err, reconcileErr)
	}
	if _, err := resource.Removed(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("removed record after capture-to-release recovery=%v, want absent", err)
	}
}

func TestSeededWorkspaceRemovalUsesImmutableReleaseActionBudget(t *testing.T) {
	_, resource := workspacefixture.NewFinalizeFixture(t)
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
