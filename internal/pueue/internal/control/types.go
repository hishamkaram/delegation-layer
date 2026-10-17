package control

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	// FixtureVersion names the serialized test fixture baseline. Runtime
	// admission accepts any nonempty observed version whose behavior matches
	// the validated command/schema contract.
	FixtureVersion            = "4.0.4"
	DefaultObservationTimeout = 5 * time.Second
	// PrivateStatusObservationTimeout bounds a private status observation,
	// including generation leases and any already-running control command.
	PrivateStatusObservationTimeout = 30 * time.Second
	// RunnerCommandUpgradeTimeout bounds one complete queued-runner repair.
	RunnerCommandUpgradeTimeout = 30 * time.Second
)

var (
	ErrSubmissionUncertain = errors.New("pueue task submission outcome is unknown")
	ErrEnqueueUncertain    = errors.New("pueue staged task enqueue outcome is unknown")
	ErrStagedCapability    = errors.New("pueue staged launch capability is unavailable")
	// ErrCleanupDeferred means the exact task row is still running while its
	// Delegate outcome is already terminal. A later maintenance pass owns
	// removal after Pueue reports the row as ended.
	ErrCleanupDeferred        = errors.New("pueue completed task cleanup deferred")
	ErrCleanupAuthorization   = errors.New("successful supervisor cleanup authorization is unavailable")
	ErrMigrationAuthorization = errors.New("private supervisor migration authorization is unavailable")
)

// Options controls caller observation and an optional explicit process context.
// Nil Resolution and Environment use current process control values at each
// fresh call; the default child environment is bounded to pueue's control
// variables. A nonnil Environment is passed through as supplied by the caller.
type Options struct {
	ObservationTimeout time.Duration
	Resolution         *ResolutionContext
	Environment        []string
	// ControlOwnerExecutable is the delegate-run executable that owns a
	// supervisor control process after the invoking CLI stops observing it.
	// Empty keeps the in-process owner used by focused package tests and
	// explicitly embedded clients.
	ControlOwnerExecutable string
	// PrivateMaintenanceLauncher starts one delegate-run process to own already
	// persisted private queue maintenance after the invoking CLI exits. Empty
	// keeps the in-process owner used by focused package tests and embedders.
	PrivateMaintenanceLauncher func(context.Context, string, string) error
	resolutionPinned           bool
	// SuccessfulCleanupAuthorizer is the application-owned policy gate for
	// exact private-supervisor cleanup. Pueue does not inspect Delegate task or
	// inspection schemas.
	SuccessfulCleanupAuthorizer func(context.Context, string, task.SupervisorRef, []Job) (PrivateQueueCleanupPlan, error)
	// PrivateTerminalEvidencePreparer retains observations while the snapshot's
	// queue identity is protected. Authorization remains a separate read-only
	// application decision after that short mutation boundary is released.
	PrivateTerminalEvidencePreparer func(context.Context, string, task.SupervisorRef, []Job) error
	// PrivateMigrationAuthorizer is the application-owned policy gate for
	// stopping a private supervisor. Pueue supplies an immutable queue snapshot;
	// it does not inspect Delegate task or inspection schemas.
	PrivateMigrationAuthorizer func(context.Context, string, task.SupervisorRef, []Job) error
	// Observer is an optional finite, concurrency-safe acceptance recorder.
	// It receives copied values and must not block process ownership.
	Observer func(CommandEvent)
	// processObserver is a deterministic test seam for read-only process
	// incarnation inspection. Production always uses the native observer.
	processObserver func(int) (privateProcessObservation, error)
	// taskObservationEntryObserver is a deterministic test seam for proving
	// that independent callers overlap one private task-status snapshot.
	taskObservationEntryObserver func()
}

// PrivateQueueCleanupPlan separates removal authority from a bounded retry
// hint. RemoveIDs are exact terminal rows authorized by application evidence.
// Settling is true only when that evidence is already terminal while its queue
// row has not yet advanced; it never authorizes a removal by itself.
type PrivateQueueCleanupPlan struct {
	RemoveIDs []int64
	Settling  bool
}

// Observation is positive only when exactly one row matches the complete binding.
// Ended describes the supervised job, never all descendants or a task outcome.
type Observation struct {
	Matched       bool
	State         State
	Group         string
	Identity      Identity
	Binding       task.SupervisorRef
	NumericTaskID int64
	Pending       *Pending
}

// StopResult keeps request, routing, acknowledgment and observation separate.
// Acknowledged is nil when there is no completed response.
type StopResult struct {
	Requested     bool
	Matched       bool
	Attempted     bool
	Action        string
	NumericTaskID *int64
	Acknowledged  *bool
	InFlight      bool
	ObservedState State
	Message       string
	Pending       *Pending
}
