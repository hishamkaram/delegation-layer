package contract

// Receipt is the exact numeric scheduling fact published before enqueue.
type Receipt struct {
	SchemaVersion int    `json:"schema_version"`
	RequestSHA256 string `json:"request_sha256"`
	NumericTaskID int64  `json:"numeric_task_id"`
}

// FailureDiagnosticName is optional private evidence for a failed helper. It
// never becomes a Git-content capture artifact or provider outcome payload.
const FailureDiagnosticName = "error.txt"
