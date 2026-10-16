package app

import (
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

const defaultWorkspacePageSize = 50

// WorkspaceSupportReport describes core behavior; it makes no authentication
// or provider-native containment claim.
type WorkspaceSupportReport struct {
	Isolation         []string `json:"isolation"`
	Source            string   `json:"source"`
	Capture           string   `json:"capture"`
	DefaultBudget     string   `json:"default_budget"`
	CheckoutRetention string   `json:"checkout_retention"`
	EvidenceRetention string   `json:"evidence_retention"`
}

func workspaceSupport() *WorkspaceSupportReport {
	return &WorkspaceSupportReport{Isolation: []string{"git-worktree"}, Source: "committed-base", Capture: "git-content", DefaultBudget: workspace.DefaultBudget.String(), CheckoutRetention: "explicit-release", EvidenceRetention: "explicit-forget"}
}

func workspaceListSummary(description WorkspaceResponse) WorkspaceResponse {
	return WorkspaceResponse{ID: description.ID, Isolation: description.Isolation, State: description.State, BaseOID: description.BaseOID, SourceChangesExcluded: description.SourceChangesExcluded, EvidenceState: description.EvidenceState, NextAction: description.NextAction}
}

func writeHumanWorkspaceResponse(output *strings.Builder, response Response) {
	if response.Workspace != nil {
		writeHumanWorkspace(output, *response.Workspace)
	}
	for _, item := range response.Workspaces {
		writeHumanWorkspace(output, item)
	}
	if response.WorkspaceCursor != "" {
		fmt.Fprintf(output, "workspace_cursor=%s\n", response.WorkspaceCursor)
	}
}

func writeHumanWorkspace(output *strings.Builder, workspace WorkspaceResponse) {
	fmt.Fprintf(output, "workspace=%s state=%s evidence=%s", workspace.ID, workspace.State, workspace.EvidenceState)
	if workspace.CanonicalCwd != "" {
		fmt.Fprintf(output, " cwd=%q", workspace.CanonicalCwd)
	}
	if workspace.OperationID != "" {
		fmt.Fprintf(output, " operation=%s", workspace.OperationID)
	}
	if workspace.NextAction != "" {
		fmt.Fprintf(output, " next_action=%s", workspace.NextAction)
	}
	output.WriteByte('\n')
}
