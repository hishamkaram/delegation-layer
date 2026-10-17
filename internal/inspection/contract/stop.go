package contract

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrStopAlreadyClaimed = errors.New("inspection stop already claimed")

type StopRequestRecord struct {
	SchemaVersion int                `json:"schema_version"`
	RequestSHA256 string             `json:"request_sha256"`
	NumericTaskID int64              `json:"numeric_task_id"`
	Supervisor    task.SupervisorRef `json:"supervisor"`
	Deadline      string             `json:"deadline"`
}

type StopReplyRecord struct {
	SchemaVersion int    `json:"schema_version"`
	StopSHA256    string `json:"stop_sha256"`
	NumericTaskID int64  `json:"numeric_task_id"`
	Action        string `json:"action"`
	Acknowledged  bool   `json:"acknowledged"`
}

type StopObservationRecord struct {
	SchemaVersion int    `json:"schema_version"`
	StopSHA256    string `json:"stop_sha256"`
	NumericTaskID int64  `json:"numeric_task_id"`
	State         string `json:"state"`
}
