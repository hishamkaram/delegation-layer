package contract

// CaptureObligation is authorized before the corresponding use can launch.
// Its typed subject prevents a verification capture being attributed to a
// provider task. The reservation, rather than conversation state, owns use.
type CaptureObligation struct {
	SchemaVersion int      `json:"schema_version"`
	WorkspaceID   string   `json:"workspace_id"`
	Sequence      uint64   `json:"sequence"`
	Subject       Subject  `json:"subject"`
	Predecessor   *Subject `json:"predecessor,omitempty"`
	BudgetNanos   int64    `json:"budget_nanos"`
}
