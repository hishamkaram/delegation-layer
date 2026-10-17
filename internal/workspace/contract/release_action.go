package contract

// ReleaseAction authorizes a Lead-requested capture and subsequent removal.
// It is not the release commit: a failed normal capture leaves use available.
type ReleaseAction struct {
	SchemaVersion int    `json:"schema_version"`
	WorkspaceID   string `json:"workspace_id"`
	ID            string `json:"id"`
	Discard       bool   `json:"discard"`
	BudgetNanos   int64  `json:"budget_nanos"`
}
