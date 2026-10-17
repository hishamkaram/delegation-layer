package app

import (
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestWorkspaceForgetIntentKeepsFirstBudgetAndDeadline(t *testing.T) {
	store, resource := workspacefixture.NewFinalizeFixture(t)
	intent, err := workspaces.EnsureForgetIntent(t.Context(), store, resource, 17*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := workspaces.EnsureForgetIntent(t.Context(), store, resource, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.OperationID != intent.OperationID || replayed.BudgetNanos != int64(17*time.Second) || replayed.Deadline != intent.Deadline {
		t.Fatalf("replay renewed original forget authorization: initial=%+v replay=%+v", intent, replayed)
	}
	result := workspaceCommandActionResponse(t.Context(), store, resource, "forget", false, workspaces.ActionAcknowledgement{}, newResponse("workspace"), workspace.ErrPending)
	if result.code != 3 || result.response.Workspace.NextAction != "watch_workspace" || result.response.Workspace.OperationID != "" {
		t.Fatalf("helperless forget intent was treated as an unaccepted action: %+v", result)
	}
}
