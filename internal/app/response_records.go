package app

import (
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	// OutputSchemaVersion is the stable public control-response schema.
	OutputSchemaVersion = 1
	defaultPublisher    = "delegate"
	// MaxJSONResponseBytes bounds each complete public JSON response. The
	// writer preserves the response's authority fields and shortens only
	// diagnostic text when the encoded response would exceed this bound.
	MaxJSONResponseBytes = task.MaxControlRecordSize
)

// ErrJSONResponseTooLarge reports that authority and descriptor fields alone
// cannot fit in the complete JSON response bound.
var ErrJSONResponseTooLarge = fmt.Errorf("%w: JSON response exceeds maximum permitted size", task.ErrControlRecordTooBig)

// Response is the bounded public control response. Payload and sealed raw
// output are described by immutable digests; live raw descriptors carry only
// rooted paths and availability. Answer bytes are streamed by a future typed
// result API rather than copied into an unbounded control object.
type Response struct {
	Interaction       *task.InteractionState     `json:"interaction,omitempty"`
	Delivery          *task.InteractionDelivery  `json:"delivery,omitempty"`
	Events            []task.InteractionEvent    `json:"events,omitempty"`
	Cursor            *uint64                    `json:"cursor,omitempty"`
	SchemaVersion     int                        `json:"schema_version"`
	Command           string                     `json:"command"`
	RootID            string                     `json:"root_id,omitempty"`
	TaskID            string                     `json:"task_id,omitempty"`
	TaskRecord        string                     `json:"task_record,omitempty"`
	ParentTaskID      string                     `json:"parent_task_id,omitempty"`
	Status            string                     `json:"status,omitempty"`
	Admission         string                     `json:"admission"`
	Liveness          string                     `json:"liveness"`
	Publication       string                     `json:"publication"`
	Outcome           *task.OutcomeRecord        `json:"outcome,omitempty"`
	Payload           *task.PayloadDescriptor    `json:"payload,omitempty"`
	Raw               []taskdir.LogDescriptor    `json:"raw,omitempty"`
	EvidenceSHA256    string                     `json:"evidence_sha256,omitempty"`
	Supervisor        *SupervisorResponse        `json:"supervisor,omitempty"`
	SupervisorControl *SupervisorControlResponse `json:"supervisor_control,omitempty"`
	Stops             []StopResponse             `json:"stops,omitempty"`
	Stop              *StopResponse              `json:"stop,omitempty"`
	Pending           *PendingResponse           `json:"pending,omitempty"`
	Capability        *CapabilityReport          `json:"capability,omitempty"`
	Continuation      *ContinuationResponse      `json:"continuation,omitempty"`
	Failure           *DispatchFailureResponse   `json:"failure,omitempty"`
	Diagnostics       []task.OutputDiagnostic    `json:"diagnostics,omitempty"`
	Recheck           *RecheckResponse           `json:"recheck,omitempty"`
	Error             string                     `json:"error,omitempty"`
	ErrorTruncated    bool                       `json:"error_truncated,omitempty"`
	Workspace         *WorkspaceResponse         `json:"workspace,omitempty"`
	Workspaces        []WorkspaceResponse        `json:"workspaces,omitempty"`
	WorkspaceCursor   string                     `json:"workspace_cursor,omitempty"`
	WorkspaceSupport  *WorkspaceSupportReport    `json:"workspace_support,omitempty"`
}

// DispatchFailureResponse gives an agent a stable recovery decision without
// exposing supervisor paths or raw process diagnostics.
type DispatchFailureResponse struct {
	Code            string                     `json:"code"`
	ReasonCode      string                     `json:"reason_code,omitempty"`
	Stage           string                     `json:"stage"`
	NextAction      string                     `json:"next_action"`
	RecoveryCommand string                     `json:"recovery_command,omitempty"`
	Inspection      *InspectionFailureResponse `json:"inspection,omitempty"`
}

// InspectionFailureResponse retains bounded, path-free evidence about an
// admission inspection failure. The task ID addresses its durable journal
// under the caller's existing state root without exposing that root.
type InspectionFailureResponse struct {
	Class         string `json:"class"`
	DeadlineState string `json:"deadline_state"`
	JournalTaskID string `json:"journal_task_id,omitempty"`
}

// ContinuationResponse is emitted when a task can be continued or when a
// continuation command creates a linked successor. Task records and sealed
// evidence remain authoritative for the detailed lifecycle.
type ContinuationResponse struct {
	Resumable       bool   `json:"resumable"`
	Mode            string `json:"mode"`
	PredecessorID   string `json:"predecessor_task_id,omitempty"`
	ContinueCommand string `json:"continue_command,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// RecheckResponse describes one immutable offline assessment while retaining
// the source task's original outcome and publication fields in Response.
type RecheckResponse struct {
	Status               string                  `json:"status"`
	AssessmentID         string                  `json:"assessment_id"`
	SourceOutcomeSHA256  string                  `json:"source_outcome_sha256"`
	SourceSealSHA256     string                  `json:"source_seal_sha256"`
	SourceEvidenceSHA256 string                  `json:"source_evidence_sha256"`
	SourcePredicate      task.PredicateRef       `json:"source_predicate"`
	Target               task.PredicateRef       `json:"target"`
	ReportPath           string                  `json:"report_path"`
	Diagnostics          []task.OutputDiagnostic `json:"diagnostics,omitempty"`
	Answer               *task.PayloadDescriptor `json:"answer,omitempty"`
	AnswerPath           string                  `json:"answer_path,omitempty"`
}

type SupervisorResponse = tasklifecycle.SupervisorObservation

// SupervisorControlResponse describes Delegate-owned supervisor recovery
// without exposing executable paths, socket paths, or process diagnostics.
type SupervisorControlResponse struct {
	Action             string `json:"action"`
	State              string `json:"state"`
	LegacyReachability string `json:"legacy_reachability"`
	ActiveWork         string `json:"active_work"`
	MigrationPhase     string `json:"migration_phase,omitempty"`
	NextAction         string `json:"next_action"`
	ReasonCode         string `json:"reason_code,omitempty"`
}

type StopResponse struct {
	RequestID        string `json:"request_id,omitempty"`
	Cause            string `json:"cause,omitempty"`
	Requested        bool   `json:"requested"`
	Matched          bool   `json:"matched"`
	Attempted        bool   `json:"attempted"`
	Action           string `json:"action,omitempty"`
	NumericTaskID    *int64 `json:"numeric_task_id,omitempty"`
	Acknowledged     *bool  `json:"acknowledged,omitempty"`
	InFlight         bool   `json:"in_flight"`
	ObservedState    string `json:"observed_state"`
	Terminated       bool   `json:"terminated"`
	Message          string `json:"message,omitempty"`
	MessageTruncated bool   `json:"message_truncated,omitempty"`
}

type PendingResponse struct {
	Kind string `json:"kind"`
}

const pendingLifetimeFinalization = "lifetime-finalization"

type commandResult struct {
	response Response
	code     int
	err      error
}
