package contract

import (
	"encoding/json"
)

// ResultReason is deliberately closed: native errors and arbitrary diagnostics
// cannot be substituted into persisted inspection evidence.
type ResultReason string

const (
	ResultEligible    ResultReason = "eligible"
	ResultUnavailable ResultReason = "unavailable"
	ResultExpired     ResultReason = "deadline-expired"
)

type ReceiptRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RequestSHA256 string `json:"request_sha256"`
	NumericTaskID *int64 `json:"numeric_task_id"`
}

type ResultRecord struct {
	SchemaVersion int                        `json:"schema_version"`
	RequestSHA256 string                     `json:"request_sha256"`
	Reason        ResultReason               `json:"reason"`
	Facts         map[string]json.RawMessage `json:"facts"`
}

// CompletionRecord proves completed local inspection work, not supervisor
// termination. Eligibility also requires the exact worker's successful exit.
type CompletionRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RequestSHA256 string `json:"request_sha256"`
	ResultSHA256  string `json:"result_sha256"`
	CompletedAt   string `json:"completed_at"`
	NativeExit    string `json:"native_exit"`
}
