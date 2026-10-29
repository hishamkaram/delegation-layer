// Package contract owns the scheduler's immutable definitions, supplied
// request values, pure validity rules, and the consumer-owned Store port. It
// performs no I/O and reads no clock: callers supply every instant and ID.
//
// This is the recurring-schedule scheduler, unrelated to the pueue supervisor
// that runs admitted tasks. Task files remain the authority for every task's
// outcome; stored observations are a cache and never authorize a launch.
package contract

import (
	"context"
	"errors"
)

// ErrConflict reports a compare-and-set that lost: the schedule state or
// cursor, or an occurrence's release, no longer matches what the caller expected.
var ErrConflict = errors.New("scheduler compare-and-set conflict: stored state does not match the expected state")

// Store is the scheduler's transactional storage port. Each method is one
// short domain transaction; no SQL handle, generic record access, or caller
// transaction crosses it. Methods validate their input with this package's
// rules. A missing record returns fs.ErrNotExist; a lost compare-and-set returns
// ErrConflict; a stored record that fails validation or digest checks returns
// task.ErrInvariantFault. After any error the commit outcome may be unknown, so
// callers re-read before acting.
type Store interface {
	// CreateSchedule stores the definition as InitialSchedule. An existing
	// schedule ID returns ErrConflict; a caller recovering from an uncertain
	// create compares the stored definition digest with its own.
	CreateSchedule(ctx context.Context, definition Definition) error
	// GetSchedule returns a full read after verifying both stored digests.
	GetSchedule(ctx context.Context, scheduleID string) (Schedule, error)
	// ListSchedules returns metadata-only summaries.
	ListSchedules(ctx context.Context, page SchedulePage) (SchedulePageResult, error)
	// ChangeState applies a compare-and-set state change and, for a resume, its
	// paused span and cursor in the same transaction. It never cancels or
	// releases an active occurrence.
	ChangeState(ctx context.Context, change StateChange) error
	// ClaimAndArm atomically inserts the armed occurrence and advances the
	// cursor of an enabled schedule with no active occurrence. Only
	// ClaimNewlyCommitted authorizes the caller's single invocation. A due time
	// that is already claimed returns ClaimExisting; an uncertain commit
	// returns ClaimUnknown with its error, even if a later read shows the row.
	ClaimAndArm(ctx context.Context, claim Claim) (ClaimResult, error)
	// RecordObservation replaces the cached observation only with a strictly
	// newer ObservedAt; an equal or older one leaves it unchanged.
	RecordObservation(ctx context.Context, observation OccurrenceObservation) error
	// Release releases an active occurrence once. A repeat is decided by
	// CheckRepeatedRelease.
	Release(ctx context.Context, release OccurrenceRelease) error
	// RecordSkip stores the span and advances the cursor in one transaction.
	RecordSkip(ctx context.Context, skip Skip) error
	// GetOccurrence returns the occurrence keyed by its task ID.
	GetOccurrence(ctx context.Context, taskID string) (Occurrence, error)
	// History returns one schedule's occurrences and skip spans together.
	History(ctx context.Context, page HistoryPage) (HistoryPageResult, error)
	// ActiveOccurrences returns occurrences that still hold their reservation.
	ActiveOccurrences(ctx context.Context, page ActivePage) (ActivePageResult, error)
	// Close releases the store's resources.
	Close() error
}
