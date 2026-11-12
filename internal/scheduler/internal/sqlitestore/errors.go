package sqlitestore

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Sanitized refusal reasons. Each joins an existing error class so callers
// classify the whole error, never the text.
var (
	errNotInitialized       = &compatibilityRefusalError{classification: CompatibilityAbsent, cause: fmt.Errorf("%w: scheduler database is not initialized", fs.ErrNotExist)}
	errForeignDatabase      = &compatibilityRefusalError{classification: CompatibilityIncompatible, cause: fmt.Errorf("%w: database is not a scheduler database", task.ErrUnsupportedSchema)}
	errNewerSchema          = &compatibilityRefusalError{classification: CompatibilityNewerSchema, cause: fmt.Errorf("%w: scheduler database schema is newer than this build", task.ErrUnsupportedSchema)}
	errJournalMode          = &compatibilityRefusalError{classification: CompatibilityIncompatible, cause: fmt.Errorf("%w: scheduler database journal mode is unsupported", task.ErrUnsupportedSchema)}
	errConnectionConfig     = fmt.Errorf("%w: scheduler connection settings do not match", task.ErrInvariantFault)
	errSchemaMismatch       = &compatibilityRefusalError{classification: CompatibilityIncompatible, cause: fmt.Errorf("%w: scheduler database schema objects do not match", task.ErrInvariantFault)}
	errRootMismatch         = &compatibilityRefusalError{classification: CompatibilityIncompatible, cause: fmt.Errorf("%w: scheduler database belongs to another state root", task.ErrIdentityMismatch)}
	errUnsafeFile           = &compatibilityRefusalError{classification: CompatibilityIncompatible, cause: fmt.Errorf("%w: scheduler path has an unsafe owner, mode, or type", task.ErrEvidenceFault)}
	errCommitOutcomeUnknown = errors.New("scheduler commit outcome is unknown")
)

// classify maps SQLite result codes to the existing error classes, keeping the
// driver error in the chain.
func classify(err error) error {
	var sqliteErr *sqlite.Error
	if err == nil || !errors.As(err, &sqliteErr) {
		return err
	}
	code := sqliteErr.Code()
	switch code & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return fmt.Errorf("%w: %w", task.ErrLockBusy, err)
	case sqlite3.SQLITE_READONLY:
		return fmt.Errorf("%w: scheduler database is read-only: %w", fs.ErrPermission, err)
	case sqlite3.SQLITE_CORRUPT:
		return fmt.Errorf("%w: %w", task.ErrInvariantFault, err)
	case sqlite3.SQLITE_NOTADB:
		return fmt.Errorf("%w: %w", errForeignDatabase, err)
	case sqlite3.SQLITE_CONSTRAINT:
		if code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY {
			return fmt.Errorf("%w: %w", contract.ErrConflict, err)
		}
		return fmt.Errorf("%w: %w", task.ErrInvariantFault, err)
	default:
		return err
	}
}

// storedFault reports a stored value that fails its contract.
func storedFault(subject string, err error) error {
	return fmt.Errorf("%w: stored %s: %w", task.ErrInvariantFault, subject, err)
}

// Compatibility is a passive assessment, not permission to migrate or execute.
type Compatibility string

const (
	CompatibilityAbsent                Compatibility = "absent"
	CompatibilityCompatible            Compatibility = "compatible"
	CompatibilityMigrationRequired     Compatibility = "migration_required"
	CompatibilityNewerSchema           Compatibility = "newer_schema"
	CompatibilityIncompatible          Compatibility = "incompatible"
	CompatibilityObservationIncomplete Compatibility = "observation_incomplete"
)

// CompatibilityOf classifies every cause, including wrapped and joined errors.
// An unrecognized observation/cleanup fault prevents a definitive assessment.
func CompatibilityOf(err error) Compatibility {
	if err == nil {
		return CompatibilityCompatible
	}
	// Only this node is authoritative here. A nested finding must not bypass
	// other joined causes, including an unknown observation or close failure.
	if finding, ok := err.(compatibilityFinding); ok {
		return finding.schedulerCompatibility()
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return compatibilityOfCauses(joined.Unwrap())
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if cause := wrapped.Unwrap(); cause != nil {
			return CompatibilityOf(cause)
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return CompatibilityAbsent
	}
	if errors.Is(err, contract.ErrMigrationRequired) {
		return CompatibilityMigrationRequired
	}
	return CompatibilityObservationIncomplete
}

// compatibilityFinding is an owner-local capability on the current error
// node. Its private method keeps classification authority inside storage.
type compatibilityFinding interface {
	schedulerCompatibility() Compatibility
}

// A refusal owns a definitive storage finding; its underlying domain error
// stays in the chain for existing callers.
type compatibilityRefusalError struct {
	classification Compatibility
	cause          error
}

func (e *compatibilityRefusalError) Error() string                         { return e.cause.Error() }
func (e *compatibilityRefusalError) Unwrap() error                         { return e.cause }
func (e *compatibilityRefusalError) schedulerCompatibility() Compatibility { return e.classification }

func compatibilityOfCauses(causes []error) Compatibility {
	var result Compatibility
	for _, cause := range causes {
		if cause == nil {
			continue
		}
		observed := CompatibilityOf(cause)
		if observed == CompatibilityObservationIncomplete || (result != "" && result != observed) {
			return CompatibilityObservationIncomplete
		}
		result = observed
	}
	if result == "" {
		return CompatibilityObservationIncomplete
	}
	return result
}
