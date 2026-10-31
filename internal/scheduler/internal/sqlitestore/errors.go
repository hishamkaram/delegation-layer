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
	errNotInitialized       = fmt.Errorf("%w: scheduler database is not initialized", fs.ErrNotExist)
	errForeignDatabase      = fmt.Errorf("%w: database is not a scheduler database", task.ErrUnsupportedSchema)
	errNewerSchema          = fmt.Errorf("%w: scheduler database schema is newer than this build", task.ErrUnsupportedSchema)
	errJournalMode          = fmt.Errorf("%w: scheduler database journal mode is unsupported", task.ErrUnsupportedSchema)
	errConnectionConfig     = fmt.Errorf("%w: scheduler connection settings do not match", task.ErrInvariantFault)
	errSchemaMismatch       = fmt.Errorf("%w: scheduler database schema objects do not match", task.ErrInvariantFault)
	errRootMismatch         = fmt.Errorf("%w: scheduler database belongs to another state root", task.ErrIdentityMismatch)
	errUnsafeFile           = fmt.Errorf("%w: scheduler path has an unsafe owner, mode, or type", task.ErrEvidenceFault)
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
