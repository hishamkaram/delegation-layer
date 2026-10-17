package app

import (
	"fmt"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

func TestWorkspaceListUsesBoundedSummaryPages(t *testing.T) {
	store, original := testsupport.NewResource(t)
	for index := 1; index <= 3; index++ {
		request := original.Request()
		request.WorkspaceID, request.TaskID = fmt.Sprintf("%032x", index), fmt.Sprintf("%032x", index+10)
		resource, err := workspace.EstablishResource(t.Context(), store, request)
		if err != nil {
			t.Fatal(err)
		}
		if err = resource.Close(); err != nil {
			t.Fatal(err)
		}
	}
	first := listManagedWorkspaces(Arguments{WorkspaceLimit: 2}, store, newResponse("workspace"))
	if first.code != 0 || len(first.response.Workspaces) != 2 || first.response.WorkspaceCursor != fmt.Sprintf("%032x", 2) {
		t.Fatalf("first summary page: %+v", first)
	}
	for _, summary := range first.response.Workspaces {
		if summary.Capture != nil || summary.Operation != nil || len(summary.Artifacts) != 0 {
			t.Fatalf("list included unbounded evidence: %+v", summary)
		}
	}
	second := listManagedWorkspaces(Arguments{WorkspaceLimit: 2, WorkspaceAfter: first.response.WorkspaceCursor}, store, newResponse("workspace"))
	if second.code != 0 || len(second.response.Workspaces) != 2 || second.response.WorkspaceCursor != "" || second.response.Workspaces[0].ID != fmt.Sprintf("%032x", 3) {
		t.Fatalf("next summary page skipped or repeated resources: %+v", second)
	}
}

func TestWorkspaceListRejectsOversizedPages(t *testing.T) {
	store, _ := testsupport.NewResource(t)
	invalid := listManagedWorkspaces(Arguments{WorkspaceLimit: defaultWorkspacePageSize + 1}, store, newResponse("workspace"))
	if invalid.code != 2 {
		t.Fatalf("invalid page size: %+v", invalid)
	}
}
