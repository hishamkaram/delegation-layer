package contract

// ForgetRef is one exact Delegate-owned ref in the forget inventory. The
// expected OID is compared by the coordinator's Git CAS operation.
type ForgetRef struct {
	Ref         string `json:"ref"`
	ExpectedOID string `json:"expected_oid"`
}

// ForgetArtifact identifies an optional workspace artifact that the
// coordinator may remove after validating this exact inventory.
type ForgetArtifact struct {
	OperationID string `json:"operation_id"`
	Name        string `json:"name"`
	Length      int64  `json:"length"`
	SHA256      string `json:"sha256"`
}

// ForgetIntent is the sole durable authority for intentionally forgetting the
// listed refs and optional workspace artifacts. It does not perform deletion.
type ForgetIntent struct {
	SchemaVersion int              `json:"schema_version"`
	WorkspaceID   string           `json:"workspace_id"`
	OperationID   string           `json:"operation_id"`
	BudgetNanos   int64            `json:"budget_nanos"`
	Deadline      string           `json:"deadline"`
	Refs          []ForgetRef      `json:"refs,omitempty"`
	Artifacts     []ForgetArtifact `json:"artifacts,omitempty"`
}

// ForgottenRecord is the immutable terminal tombstone for one forget intent.
type ForgottenRecord struct {
	SchemaVersion int    `json:"schema_version"`
	WorkspaceID   string `json:"workspace_id"`
	OperationID   string `json:"operation_id"`
}
