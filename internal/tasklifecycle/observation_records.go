package tasklifecycle

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type PendingKind string

const (
	PendingSupervisor           PendingKind = "supervisor-command"
	PendingLifetimeFinalization PendingKind = "lifetime-finalization"
	PendingInteraction          PendingKind = "interaction"
	PendingPublication          PendingKind = "publication"
)

// Observation is a detached view of task journals and completed observations.
// It carries no authorization or mutable runtime owner. Each workflow keeps
// its transient observation locally; durable records remain authoritative.
type Observation struct {
	// Deferral describes this attempt only. A later attempt gets its own
	// preparation; this output never grants or suppresses its mutations.
	MaintenanceDeferred bool
	MaintenanceError    error
	Admission           task.Admission
	AdmissionObserved   bool
	Liveness            task.Liveness
	LivenessObserved    bool
	Inspection          *taskdir.TaskInspection
	Interaction         *task.InteractionState
	Raw                 []taskdir.LogDescriptor
	Supervisor          *SupervisorObservation
	Stops               []taskdir.StopRecord
	StopsRead           bool
	Pending             PendingKind
	Continuation        *ContinuationOffer
}

// Interrupted reports a closure-proven interruption that can never publish.
func (o Observation) Interrupted() bool {
	return o.Continuation != nil && o.Continuation.Cause == ContinuationInterrupted
}

type SupervisorObservation struct {
	Matched       bool   `json:"matched"`
	State         string `json:"state"`
	NumericTaskID *int64 `json:"numeric_task_id,omitempty"`
}

// ContinuationCause names the durable fact that ended a provider turn without
// an authoritative outcome and is the observed task status.
type ContinuationCause string

const (
	// ContinuationTimedOut is a terminated budget stop.
	ContinuationTimedOut ContinuationCause = "timed_out"
	// ContinuationInterrupted is a canonical closed native lifetime with no
	// seal or outcome, for example after a machine reboot. It proves that the
	// old scope ended, not provider success or publication.
	ContinuationInterrupted ContinuationCause = "interrupted"
)

// ContinuationSessionSource names where a continuation's provider session
// reference came from.
type ContinuationSessionSource string

const (
	// ContinuationSessionObserved is the predecessor's validated provider identity.
	ContinuationSessionObserved ContinuationSessionSource = "observed"
	// ContinuationSessionRecordedInput is the predecessor's own validated
	// requested PriorSession, used only when no provider identity was recorded.
	ContinuationSessionRecordedInput ContinuationSessionSource = "recorded_input"
)

// ContinuationOffer explains an observed timeout or interruption. The caller
// formats its own command from the retained runner and supervisor coordinates.
type ContinuationOffer struct {
	Cause         ContinuationCause
	Resumable     bool
	Mode          string
	PredecessorID string
	SessionSource ContinuationSessionSource
	Reason        string
	CommandRunner string
	Supervisor    *task.SupervisorRef
	// Cancelled is derived from a validated durable user stop. It does not
	// change Cause; it lets a presentation edge say the task was cancelled.
	Cancelled bool
}

// StatusResult distinguishes control-plane uncertainty from earlier store faults.
type StatusResult struct {
	Observation
	ControlObservation bool
}

// CollectionResult reports whether the finite observation ended while pending.
// A terminal outcome may still carry an earlier supervisor-pending annotation.
type CollectionResult struct {
	Observation
	Waiting    bool
	Completion *CollectionCompletion
}

// CollectionCompletion records terminal finalization in its existing order:
// publication, optional timeout observation, session release, recovery offer.
// The presentation edge can retain that order without repeating journal reads.
type CollectionCompletion struct {
	Outcome           *task.OutcomeRecord
	CollectionError   error
	Continuation      *ContinuationOffer
	ReleaseError      error
	RecoveryAvailable *bool
}
