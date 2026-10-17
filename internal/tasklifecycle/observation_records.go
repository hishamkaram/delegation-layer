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

type SupervisorObservation struct {
	Matched       bool   `json:"matched"`
	State         string `json:"state"`
	NumericTaskID *int64 `json:"numeric_task_id,omitempty"`
}

// ContinuationOffer explains an observed timeout. The caller formats its own
// command from the retained runner and supervisor coordinates.
type ContinuationOffer struct {
	Resumable     bool
	Mode          string
	PredecessorID string
	Reason        string
	CommandRunner string
	Supervisor    *task.SupervisorRef
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
