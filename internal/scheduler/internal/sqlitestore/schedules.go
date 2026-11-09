package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// CreateSchedule stores the definition as its initial schedule. An existing
// schedule ID is a conflict.
func (s *Store) CreateSchedule(ctx context.Context, definition contract.Definition) error {
	initial, err := contract.InitialSchedule(definition)
	if err != nil {
		return err
	}
	stored := initial.Definition
	digests, err := contract.DefinitionDigests(stored)
	if err != nil {
		return err
	}
	request, err := contract.MarshalRequest(stored.Request)
	if err != nil {
		return err
	}
	created := stored.CreatedAt.UnixNano()
	return s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, `INSERT INTO schedules (schedule_id, created_at_unix_nanos, cron, timezone,
	request_json, brief, brief_sha256, definition_sha256, state, cursor_unix_nanos)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			stored.ScheduleID, created, stored.Cron, stored.Timezone, request, stored.Brief,
			digests.BriefSHA256, digests.DefinitionSHA256, string(initial.State), created)
		if execErr != nil {
			return classify(execErr)
		}
		if stored.Hook != nil {
			encoded, err := json.Marshal(stored.Hook)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO schedule_hooks(schedule_id,hook_json) VALUES (?,?)", stored.ScheduleID, encoded)
			return classify(err)
		}
		return nil
	})
}

// GetSchedule returns a full read after verifying both stored digests.
func (s *Store) GetSchedule(ctx context.Context, scheduleID string) (contract.Schedule, error) {
	if err := validScheduleID(scheduleID); err != nil {
		return contract.Schedule{}, err
	}
	var schedule contract.Schedule
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		var row scheduleRow
		var brief []byte
		scanErr := tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+`, brief FROM schedules WHERE schedule_id = ?`,
			scheduleID).Scan(append(row.targets(), &brief)...)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return fmt.Errorf("schedule %s: %w", scheduleID, fs.ErrNotExist)
		}
		if scanErr != nil {
			return classify(scanErr)
		}
		var decodeErr error
		schedule, decodeErr = row.schedule(brief)
		return decodeErr
	})
	if err != nil {
		return contract.Schedule{}, err
	}
	return schedule, nil
}

// ListSchedules returns metadata-only summaries in schedule ID order.
func (s *Store) ListSchedules(ctx context.Context, page contract.SchedulePage) (contract.SchedulePageResult, error) {
	if err := contract.ValidateSchedulePage(page); err != nil {
		return contract.SchedulePageResult{}, err
	}
	var result contract.SchedulePageResult
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		result = contract.SchedulePageResult{}
		rows, queryErr := tx.QueryContext(ctx, `SELECT `+scheduleColumns+`, length(brief) FROM schedules
	WHERE schedule_id > ? ORDER BY schedule_id LIMIT ?`, page.AfterScheduleID, page.Limit+1)
		if queryErr != nil {
			return classify(queryErr)
		}
		return scanEach(rows, func(rows *sql.Rows) error {
			var row scheduleRow
			var briefLength int64
			if scanErr := rows.Scan(append(row.targets(), &briefLength)...); scanErr != nil {
				return scanErr
			}
			if len(result.Schedules) == page.Limit {
				result.More = true
				return nil
			}
			summary, decodeErr := row.summary(briefLength)
			result.Schedules = append(result.Schedules, summary)
			return decodeErr
		})
	})
	if err != nil {
		return contract.SchedulePageResult{}, err
	}
	return result, nil
}

// ChangeState applies a compare-and-set state change and, for a resume, its
// paused span and cursor advance in the same transaction.
func (s *Store) ChangeState(ctx context.Context, change contract.StateChange) error {
	if err := contract.ValidateStateChange(change); err != nil {
		return err
	}
	return s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		state, cursor, err := scheduleState(ctx, tx, change.ScheduleID)
		if err != nil {
			return err
		}
		if state != change.From {
			return fmt.Errorf("%w: schedule %s is %s, not %s", contract.ErrConflict, change.ScheduleID, state, change.From)
		}
		if change.Resume == nil {
			return updateOne(ctx, tx, `UPDATE schedules SET state = ? WHERE schedule_id = ? AND state = ?`,
				string(change.To), change.ScheduleID, string(change.From))
		}
		resume := *change.Resume
		if err := insertSkipSpan(ctx, tx, change.ScheduleID, cursor, resume.ExpectedCursor, resume.Span()); err != nil {
			return err
		}
		return updateOne(ctx, tx, `UPDATE schedules SET state = ?, cursor_unix_nanos = ?
	WHERE schedule_id = ? AND state = ? AND cursor_unix_nanos = ?`,
			string(change.To), resume.To.UnixNano(), change.ScheduleID, string(change.From), cursor)
	})
}

// scheduleState reads a schedule's validated mutable state and cursor.
func scheduleState(ctx context.Context, tx *sql.Tx, scheduleID string) (contract.State, int64, error) {
	var state string
	var cursor int64
	err := tx.QueryRowContext(ctx, `SELECT state, cursor_unix_nanos FROM schedules WHERE schedule_id = ?`,
		scheduleID).Scan(&state, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, fmt.Errorf("schedule %s: %w", scheduleID, fs.ErrNotExist)
	}
	if err != nil {
		return "", 0, classify(err)
	}
	if err := contract.ValidateState(contract.State(state)); err != nil {
		return "", 0, storedFault("schedule state", err)
	}
	return contract.State(state), cursor, nil
}

// insertSkipSpan records span after checking the stored cursor against the
// caller's expected cursor; the caller then advances the cursor to span.To.
func insertSkipSpan(ctx context.Context, tx *sql.Tx, scheduleID string, cursor int64, expected time.Time, span contract.SkipSpan) error {
	if cursor != expected.UnixNano() {
		return fmt.Errorf("%w: schedule %s cursor moved", contract.ErrConflict, scheduleID)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO skip_spans (schedule_id, from_unix_nanos, to_unix_nanos, reason)
	VALUES (?, ?, ?, ?)`, scheduleID, span.From.UnixNano(), span.To.UnixNano(), string(span.Reason))
	return classify(err)
}

// updateOne executes a conditional update that must change exactly one row;
// zero rows means the compare-and-set lost. args pass through unchanged to
// database/sql, whose parameter API is typed any.
func updateOne(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return classify(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return classify(err)
	}
	if changed != 1 {
		return fmt.Errorf("%w: %d rows changed", contract.ErrConflict, changed)
	}
	return nil
}

func validScheduleID(scheduleID string) error {
	if err := task.ValidateTaskID(scheduleID); err != nil {
		return fmt.Errorf("schedule id: %w", err)
	}
	return nil
}
