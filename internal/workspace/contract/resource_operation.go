package contract

type OperationReference struct {
	SchemaVersion int    `json:"schema_version"`
	WorkspaceID   string `json:"workspace_id"`
	OperationID   string `json:"operation_id"`
	Kind          string `json:"kind"`
}
