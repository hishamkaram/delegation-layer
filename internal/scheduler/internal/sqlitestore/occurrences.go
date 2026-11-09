package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ClaimAndArm atomically inserts the armed occurrence and advances the cursor
// of an enabled schedule with no active occurrence. Only a commit this call
// made returns ClaimNewlyCommitted; every error returns the zero ClaimUnknown.
func (s *Store) ClaimAndArm(ctx context.Context, claim contract.Claim) (contract.ClaimResult, error) {
	if err := contract.ValidateClaim(claim); err != nil {
		return contract.ClaimResult{}, err
	}
	var existing string
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		existing = ""
		found, err := claimedTask(ctx, tx, claim)
		if err != nil || found != "" {
			existing = found
			return err
		}
		return armOccurrence(ctx, tx, claim)
	})
	switch {
	case err != nil:
		return contract.ClaimResult{}, err
	case existing != "":
		return contract.ClaimResult{Outcome: contract.ClaimExisting, TaskID: existing}, nil
	default:
		return contract.ClaimResult{Outcome: contract.ClaimNewlyCommitted, TaskID: claim.TaskID}, nil
	}
}

// claimedTask returns the task that already holds the claimed due time.
func claimedTask(ctx context.Context, tx *sql.Tx, claim contract.Claim) (string, error) {
	var taskID string
	err := tx.QueryRowContext(ctx, `SELECT task_id FROM occurrences WHERE schedule_id = ? AND due_unix_nanos = ?`,
		claim.ScheduleID, claim.Due.UnixNano()).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", classify(err)
	}
	if err := task.ValidateTaskID(taskID); err != nil {
		return "", storedFault("occurrence task id", err)
	}
	return taskID, nil
}

func armOccurrence(ctx context.Context, tx *sql.Tx, claim contract.Claim) error {
	state, cursor, err := scheduleState(ctx, tx, claim.ScheduleID)
	if err != nil {
		return err
	}
	if state != contract.StateEnabled {
		return fmt.Errorf("%w: schedule %s is %s", contract.ErrConflict, claim.ScheduleID, state)
	}
	if cursor != claim.ExpectedCursor.UnixNano() {
		return fmt.Errorf("%w: schedule %s cursor moved", contract.ErrConflict, claim.ScheduleID)
	}
	if err := quotaClaimAllowed(ctx, tx, claim.ScheduleID, claim.Due); err != nil {
		return err
	}
	var active int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM occurrences
	WHERE schedule_id = ? AND released_at_unix_nanos IS NULL`, claim.ScheduleID).Scan(&active); err != nil {
		return classify(err)
	}
	if active != 0 {
		return fmt.Errorf("%w: schedule %s already has an active occurrence", contract.ErrConflict, claim.ScheduleID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO occurrences (task_id, schedule_id, due_unix_nanos, armed_at_unix_nanos)
	VALUES (?, ?, ?, ?)`, claim.TaskID, claim.ScheduleID, claim.Due.UnixNano(), claim.ArmedAt.UnixNano()); err != nil {
		return classify(err)
	}
	return updateOne(ctx, tx, `UPDATE schedules SET cursor_unix_nanos = ?
	WHERE schedule_id = ? AND state = 'enabled' AND cursor_unix_nanos = ?`,
		claim.Due.UnixNano(), claim.ScheduleID, cursor)
}

// RecordObservation replaces the cached observation only with a strictly
// newer one; an equal or older observation leaves it unchanged.
func (s *Store) RecordObservation(ctx context.Context, record contract.OccurrenceObservation) error {
	if err := contract.ValidateOccurrenceObservation(record); err != nil {
		return err
	}
	observation := record.Observation
	return s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE occurrences SET observed_at_unix_nanos = ?, admission = ?,
	liveness = ?, publication = ?, outcome_sha256 = ?, fault_code = ?
	WHERE task_id = ? AND (observed_at_unix_nanos IS NULL OR observed_at_unix_nanos < ?)`,
			observation.ObservedAt.UnixNano(), int64(observation.Admission), int64(observation.Liveness),
			int64(observation.Publication), nullableText(observation.OutcomeSHA256), nullableText(observation.FaultCode),
			record.TaskID, observation.ObservedAt.UnixNano())
		if err != nil {
			return classify(err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 1 {
			return classify(err)
		}
		_, err = readOccurrence(ctx, tx, record.TaskID)
		return err
	})
}

// Release records the occurrence's one immutable release. A repeat is decided
// by contract.CheckRepeatedRelease against the stored release.
func (s *Store) Release(ctx context.Context, record contract.OccurrenceRelease) error {
	if err := contract.ValidateOccurrenceRelease(record); err != nil {
		return err
	}
	release := record.Release
	return s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE occurrences SET released_at_unix_nanos = ?, release_kind = ?, release_ref = ?
	WHERE task_id = ? AND released_at_unix_nanos IS NULL`,
			release.ReleasedAt.UnixNano(), string(release.Kind), release.Ref, record.TaskID)
		if err != nil {
			return classify(err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 1 {
			return classify(err)
		}
		stored, err := readOccurrence(ctx, tx, record.TaskID)
		if err != nil {
			return err
		}
		if stored.Release == nil {
			return fmt.Errorf("%w: occurrence %s release did not apply", task.ErrInvariantFault, record.TaskID)
		}
		return contract.CheckRepeatedRelease(*stored.Release, release)
	})
}

// GetOccurrence returns the occurrence keyed by its task ID.
func (s *Store) GetOccurrence(ctx context.Context, taskID string) (contract.Occurrence, error) {
	if err := task.ValidateTaskID(taskID); err != nil {
		return contract.Occurrence{}, fmt.Errorf("occurrence task id: %w", err)
	}
	var occurrence contract.Occurrence
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		var readErr error
		occurrence, readErr = readOccurrence(ctx, tx, taskID)
		return readErr
	})
	if err != nil {
		return contract.Occurrence{}, err
	}
	return occurrence, nil
}

// ActiveOccurrences returns occurrences that still hold their reservation, in
// schedule ID order; a schedule has at most one.
func (s *Store) ActiveOccurrences(ctx context.Context, page contract.ActivePage) (contract.ActivePageResult, error) {
	if err := contract.ValidateActivePage(page); err != nil {
		return contract.ActivePageResult{}, err
	}
	var result contract.ActivePageResult
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		occurrences, more, err := queryOccurrences(ctx, tx, page.Limit, `SELECT `+occurrenceColumns+` FROM occurrences
	WHERE released_at_unix_nanos IS NULL AND schedule_id > ? ORDER BY schedule_id LIMIT ?`,
			page.AfterScheduleID, page.Limit+1)
		result = contract.ActivePageResult{Occurrences: occurrences, More: more}
		return err
	})
	if err != nil {
		return contract.ActivePageResult{}, err
	}
	return result, nil
}

func readOccurrence(ctx context.Context, tx *sql.Tx, taskID string) (contract.Occurrence, error) {
	var row occurrenceRow
	err := tx.QueryRowContext(ctx, `SELECT `+occurrenceColumns+` FROM occurrences WHERE task_id = ?`, taskID).Scan(row.targets()...)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.Occurrence{}, fmt.Errorf("occurrence %s: %w", taskID, fs.ErrNotExist)
	}
	if err != nil {
		return contract.Occurrence{}, classify(err)
	}
	return row.occurrence()
}

// queryOccurrences decodes at most limit occurrences and reports whether the
// query returned one more. args pass through to database/sql.
func queryOccurrences(ctx context.Context, tx *sql.Tx, limit int, query string, args ...any) ([]contract.Occurrence, bool, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, classify(err)
	}
	var occurrences []contract.Occurrence
	more := false
	err = scanEach(rows, func(rows *sql.Rows) error {
		var row occurrenceRow
		if scanErr := rows.Scan(row.targets()...); scanErr != nil {
			return scanErr
		}
		if len(occurrences) == limit {
			more = true
			return nil
		}
		occurrence, decodeErr := row.occurrence()
		occurrences = append(occurrences, occurrence)
		return decodeErr
	})
	return occurrences, more, err
}

func nullableText(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}
