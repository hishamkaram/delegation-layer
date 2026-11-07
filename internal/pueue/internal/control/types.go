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
	ErrInfrastructureOwnerRequired = errors.New("pueue daemon infrastructure owner is required")
	ErrSubmissionUncertain         = errors.New("pueue task submission outcome is unknown")
	ErrEnqueueUncertain            = errors.New("pueue staged task enqueue outcome is unknown")
	ErrStagedCapability            = errors.New("pueue staged launch capability is unavailable")
	ErrCleanupAuthorization        = errors.New("successful supervisor cleanup authorization is unavailable")
	ErrMigrationAuthorization      = errors.New("private supervisor migration authorization is unavailable")
)

// DaemonStartGuard owns placement authorization for one synchronous start.
// Implementations must invoke start synchronously, at most once, and return its error.
// Pueue retains executable selection, identity verification, readiness and Wait.
type DaemonStartGuard interface {
	GuardDaemonStart(ctx context.Context, operationRoot string, start func() error) error
}

// PrivateGenerationGuard owns current resident authority for one publication.
// Implementations invoke commit synchronously, at most once, and return its
// error. The callback contains only canonical publication writes, never daemon
// start, orchestration, or another generation guard.
type PrivateGenerationGuard interface {
	GuardPrivateGenerationCommit(ctx context.Context, operationRoot string, commit func() error) error
}

// DaemonWaitOwner observes completion of the exact child owned by Pueue.
// The PID is diagnostic identity, not scheduling or process-mutation authority.
// Registering a handle never transfers Wait ownership or joins Client.Close.
type DaemonWaitOwner interface {
	RegisterDaemonWait(root string, pid int, done <-chan struct{}) error
}

// Options controls caller observation and an optional explicit process context.
// Nil Resolution and Environment use current process control values at each
// fresh call; the default child environment is bounded to pueue's control
// variables. A nonnil Environment is passed through as supplied by the caller.
type Options struct {
	// RuntimeClientExecutable is the installation resolver's selected transport.
	// It never changes the persisted generation binding or grants Start authority.
	// Empty retains an ordinary recorded installation; historical managed copies
	// require an explicit installed selection and are never executed implicitly.
	RuntimeClientExecutable string
	// PrivateTerminalEventRunner is the selected installed callback for a new
	// private generation. A live generation keeps its published callback string.
	PrivateTerminalEventRunner string
	// PrivateGenerationGuard is required for new private generation decisions.
	// Warm reads and exact durable publication replay require no grant.
	PrivateGenerationGuard PrivateGenerationGuard
	// DaemonStartGuard is required whenever an operation starts a daemon.
	DaemonStartGuard DaemonStartGuard
	// DaemonWaitOwner optionally retains completion handles for resident shutdown.
	DaemonWaitOwner    DaemonWaitOwner
	ObservationTimeout time.Duration
	Resolution         *ResolutionContext
	Environment        []string
	// WaitForPendingClose is selected by a resident application owner that must
	// join retained command/maintenance work before returning its admission slot
	// or lease. It reuses Close's natural completion boundary; ordinary finite
	// CLI observers keep asynchronous CloseAfterPending behavior by default.
	WaitForPendingClose bool
	// ControlOwnerExecutable is the delegate-run executable that owns a
	// supervisor control process after the invoking CLI stops observing it.
	// Empty keeps the in-process owner used by focused package tests and
	// explicitly embedded clients.
	ControlOwnerExecutable string
	// PrivateMaintenanceLauncher starts one delegate-run process to own already
	// persisted private queue maintenance after the invoking CLI exits. Empty
	// keeps the in-process owner used by focused package tests and embedders.
	PrivateMaintenanceLauncher func(context.Context, string, string, []string) error
	// PrivateMaintenanceEnvironment is the immutable launch snapshot selected
	// by composition. It is separate from the client's control environment.
	// Nil preserves standalone defaults; nonnil empty inherits nothing.
	PrivateMaintenanceEnvironment []string
	resolutionPinned              bool
	// SuccessfulCleanupAuthorizer is the application-owned policy gate for
	// exact private-supervisor cleanup. Pueue does not inspect Delegate task or
	// inspection schemas.
	SuccessfulCleanupAuthorizer func(context.Context, string, task.SupervisorRef, []Job) (PrivateQueueCleanupPlan, error)
	// PrivateTerminalEvidencePreparer retains observations while the snapshot's
	// queue identity is protected. Authorization remains a separate read-only
	// application decision after that short mutation boundary is released.
	PrivateTerminalEvidencePreparer func(context.Context, string, task.SupervisorRef, []Job) error
	// PrivateTerminalCustodyReleaser is the application-owned custody owner for
	// one elected maintenance pass. It runs with neither queue identity nor the
	// generation lease held, so custody filesystem work never blocks lifecycle
	// transitions or submissions: once per full-recovery generation before the
	// queue is observed, and after each capture before removal reacquires both.
	// It grants no removal authority. Its fault fails the attempt without
	// withholding rows the application authorizer still approves. Inline queue
	// cleanup by other consumers never calls it. Nil skips custody release.
	PrivateTerminalCustodyReleaser func(context.Context, string, task.SupervisorRef, PrivateTerminalCustodyScope) error
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
	// queueWriterWaitObserver is a deterministic test seam for proving that a
	// queue identity reader reached its wait on an active writer intent. It
	// only reports that decision and must not block.
	queueWriterWaitObserver func()
	// pendingCloseObserver is a deterministic test seam reporting resident
	// close entry with a retained command. It must not block ownership.
	pendingCloseObserver func()
}

// PrivateQueueCleanupPlan separates removal authority from a bounded retry
// hint. RemoveIDs are exact terminal rows authorized by application evidence.
// Settling is true only when that evidence is already terminal while its queue
// row has not yet advanced; it never authorizes a removal by itself.
type PrivateQueueCleanupPlan struct {
	RemoveIDs []int64
	Settling  bool
}

// PrivateTerminalCustodyScope is the work one maintenance phase hands to the
// custody owner. Candidates is a copy of the snapshot rows selected by one
// capture: a full capture can include active, unrelated, and duplicate-label
// witnesses, and preparation may have skipped or deferred any row. A candidate
// carries no terminal, closure, or release authority; the consumer validates
// its canonical records before releasing anything. Full marks the separate
// full-recovery phase: it runs once per full-recovery generation with no
// candidates, before the supervisor endpoint is verified or the queue
// captured, so custody that needs no queue row is reconciled even when the
// queue is unavailable. Capture phases, including settling recaptures, are
// never Full.
type PrivateTerminalCustodyScope struct {
	Candidates []Job
	Full       bool
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
