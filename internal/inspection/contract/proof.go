package contract

type WorkerObservationRecord struct {
	SchemaVersion    int    `json:"schema_version"`
	RequestSHA256    string `json:"request_sha256"`
	CompletionSHA256 string `json:"completion_sha256,omitempty"`
	NumericTaskID    int64  `json:"numeric_task_id"`
	State            string `json:"state"`
}
