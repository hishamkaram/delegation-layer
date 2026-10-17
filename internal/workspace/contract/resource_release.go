package contract

type ReleaseRecord struct {
	SchemaVersion int    `json:"schema_version"`
	WorkspaceID   string `json:"workspace_id"`
	OperationID   string `json:"operation_id"`
	Discard       bool   `json:"discard"`
}

// RemovedRecord is an immutable success fact published only after the
// coordinator has completed and verified the exact Git removal.
type RemovedRecord struct {
	SchemaVersion int    `json:"schema_version"`
	WorkspaceID   string `json:"workspace_id"`
	OperationID   string `json:"operation_id"`
}
