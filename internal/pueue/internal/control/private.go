package control

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	privateSupervisorDirectory           = ".supervisor"
	privateMigrationRecordName           = "migration.json"
	privateMigrationHistoryRecordName    = "migration-history.json"
	privateSupervisorBindingRecordName   = "supervisor.identity.json"
	privateBootstrapRecordName           = "bootstrap.pending"
	privateIdentityPublicationRecordName = "identity.pending.json"
	privateSchedulingEvidenceRecordName  = "ordinary-scheduling.json"
	privateTransitionLockName            = "transition.lock"
	privateQueueMaintenanceLockName      = "queue-maintenance.lock"
	privateQueueWriterIntentLockName     = "queue-writer-intent.lock"
	privateQueueIdentityGateLockName     = "queue-identity-gate.lock"
	privateQueueIdentityLockName         = "queue-identity.lock"
	privateQueueWriterIdleState          = "0"
	privateQueueWriterWaitingState       = "1"
	privateRecordStagePrefix             = ".private-record-"
)

var (
	// ErrPrivateSupervisorMigrationBusy means the private supervisor still
	// owns queued, running, or uncertain work and must remain alive.
	ErrPrivateSupervisorMigrationBusy = errors.New("private supervisor migration has active work")
	// ErrPrivateSupervisorMigrationRequired means private supervisor launch
	// provenance could not be migrated safely by the current admission path.
	ErrPrivateSupervisorMigrationRequired = errors.New("private supervisor migration is required")
	// ErrPrivateSupervisorTransitionIncomplete classifies a completion check
	// that refused an operation before mutation. A deferred caller must still
	// obtain a fresh canonical observation; this error grants no authority.
	ErrPrivateSupervisorTransitionIncomplete error = privateTransitionIncompleteError{}
	// ErrPrivateSupervisorSubmissionIncomplete retains the binding-failure
	// contract of a refused submission without changing other completion checks.
	ErrPrivateSupervisorSubmissionIncomplete error = privateTransitionIncompleteError{submission: true}
	// ErrPrivateSupervisorNotInitialized means a shared lifecycle observation
	// found no published bootstrap boundary. Callers may elect an exclusive
	// transition to initialize it, but must not create state from the read path.
	ErrPrivateSupervisorNotInitialized = errors.New("private supervisor lifecycle is not initialized")
	errStatusControlLimit              = errors.New("pueue supervisor status output limit exceeded")
	errSupervisorLockBusy              = errors.New("private supervisor lock is busy")
)

// The leaf identifies only the refused completion check. Generic binding,
// pending-command, and cleanup failures remain separate error-tree leaves.
type privateTransitionIncompleteError struct {
	submission bool
}

func (err privateTransitionIncompleteError) Error() string {
	if err.submission {
		return ErrBinding.Error()
	}
	return ErrPrivateSupervisorMigrationRequired.Error() + ": published private supervisor transition is incomplete"
}

func (err privateTransitionIncompleteError) Is(target error) bool {
	if target == ErrPrivateSupervisorTransitionIncomplete {
		return true
	}
	if err.submission {
		return target == ErrBinding
	}
	return target == ErrPrivateSupervisorMigrationRequired
}

type privateProbeScope uint8

const (
	privateProbeSnapshot privateProbeScope = iota
	privateProbeGroups
	privateProbeEndpoint
)

// Fresh bootstrap observes the supervisor once while binding the client, twice
// while checking an existing endpoint, and twice while checking a daemon just
// started by this process. Saved-binding recovery adds two exact binding
// probes before a restart, so the shared budget covers that maximum path.
const (
	privateBootstrapObservationPhases      = 7
	privateReconciliationObservationPhases = 2
	// Queue maintenance can wait for the current generation lease, observe the
	// complete queue, let terminal rows settle, and remove the exact authorized
	// rows. Give each phase one observation window while keeping the window
	// independent from a longer admission timeout.
	privateQueueMaintenanceObservationPhases = 3
	privateQueueSettlementPoll               = 25 * time.Millisecond
	// Migration performs one current-pair preparation, legacy binding and
	// queue inspection, shutdown acknowledgement and endpoint drain, then
	// successor startup and readiness reconciliation. Keep this explicit so
	// the outer deadline is derived from the full transition rather than one
	// bootstrap attempt.
	privateMigrationObservationPhases = 8
)

type privateSupervisorBindingRecord struct {
	SchemaVersion int                `json:"schema_version"`
	Binding       task.SupervisorRef `json:"binding"`
}

type privateIdentityPublicationRecord struct {
	SchemaVersion int                `json:"schema_version"`
	Binding       task.SupervisorRef `json:"binding"`
}

type privateBootstrapRecord struct {
	SchemaVersion int                       `json:"schema_version"`
	Restart       *privatePairRestartIntent `json:"restart,omitempty"`
}

type privateQueueMaintenanceRun struct {
	worker      *Client
	stateRoot   string
	private     bool
	scheduled   bool
	lockMode    int
	passTimeout time.Duration
}

type bootstrapLock struct {
	mu          sync.Mutex
	file        *os.File
	transferred bool
	closed      bool
}
