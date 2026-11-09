package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (s *Store) QuotaPolicy(ctx context.Context, id string) (contract.QuotaPolicy, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return contract.QuotaPolicy{}, err
	}
	var policy contract.QuotaPolicy
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		_, _, err := scheduleState(ctx, tx, id)
		if err != nil {
			return err
		}
		policy, _, err = readQuotaPolicy(ctx, tx, id)
		return err
	})
	return policy, err
}

func readQuotaPolicy(ctx context.Context, tx *sql.Tx, id string) (contract.QuotaPolicy, int64, error) {
	p := contract.DefaultQuotaPolicy()
	var enabled int
	var maxHold, floor int64
	err := tx.QueryRowContext(ctx, "SELECT enabled,max_hold,revision,source_floor FROM schedule_policy WHERE schedule_id = ?", id).Scan(&enabled, &maxHold, &p.Revision, &floor)
	if errors.Is(err, sql.ErrNoRows) {
		return p, 0, nil
	}
	if err != nil {
		return p, 0, classify(err)
	}
	p.Enabled = enabled == 1
	p.MaxHold = time.Duration(maxHold)
	if err := contract.ValidateQuotaPolicy(p); err != nil {
		return p, 0, storedFault("quota policy", err)
	}
	return p, floor, nil
}

func (s *Store) SetQuotaPolicy(ctx context.Context, id string, p contract.QuotaPolicy) error {
	if err := task.ValidateTaskID(id); err != nil {
		return err
	}
	if err := contract.ValidateQuotaPolicy(p); err != nil {
		return err
	}
	return s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		state, cursor, err := scheduleState(ctx, tx, id)
		if err != nil {
			return err
		}
		if state == contract.StateDeleted {
			return contract.ErrConflict
		}
		old, floor, err := readQuotaPolicy(ctx, tx, id)
		if err != nil {
			return err
		}
		if old.Revision != p.Revision {
			return contract.ErrConflict
		}
		if p.Enabled && !old.Enabled {
			floor = max(floor, cursor)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO schedule_policy(schedule_id,enabled,max_hold,revision,source_floor) VALUES (?,?,?,?,?) ON CONFLICT(schedule_id) DO UPDATE SET enabled=excluded.enabled,max_hold=excluded.max_hold,revision=excluded.revision,source_floor=excluded.source_floor`, id, p.Enabled, int64(p.MaxHold), p.Revision+1, floor)
		return classify(err)
	})
}

func (s *Store) QuotaHold(ctx context.Context, id string) (*contract.QuotaHold, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	var hold *contract.QuotaHold
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		hold, err = readQuotaHold(ctx, tx, id)
		return err
	})
	return hold, err
}

func readQuotaHold(ctx context.Context, tx *sql.Tx, id string) (*contract.QuotaHold, error) {
	var h contract.QuotaHold
	var created, until int64
	var capped int
	err := tx.QueryRowContext(ctx, "SELECT source_task_id,created_at,until_at,capped FROM schedule_holds WHERE schedule_id = ?", id).Scan(&h.SourceTaskID, &created, &until, &capped)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, classify(err)
	}
	h.Reason = "provider_quota_exhausted"
	h.CreatedAt = contract.InstantFromUnixNanos(created)
	h.Until = contract.InstantFromUnixNanos(until)
	h.Capped = capped == 1
	if err := task.ValidateTaskID(h.SourceTaskID); err != nil {
		return nil, storedFault("quota hold", err)
	}
	if err := contract.ValidateInstant("hold created", h.CreatedAt); err != nil {
		return nil, storedFault("quota hold", err)
	}
	if !h.Until.After(h.CreatedAt) {
		return nil, storedFault("quota hold", errors.New("invalid hold interval"))
	}
	return &h, nil
}

// ConsiderQuotaHold runs only against validated projection rows in this
// transaction. Any unresolved later occurrence defers the source, without moving
// its floor. The cursor and all coverage are re-read under the same writer lock.
type quotaSource struct {
	TaskID, ScheduleID string
	Due, Reset         int64
}

func readQuotaSource(ctx context.Context, tx *sql.Tx, taskID string) (quotaSource, error) {
	source := quotaSource{TaskID: taskID}
	err := tx.QueryRowContext(ctx, `SELECT o.schedule_id,o.due_unix_nanos,r.quota_until FROM occurrences o JOIN occurrence_results r ON r.task_id=o.task_id WHERE o.task_id=? AND r.verdict='rejected' AND r.code='provider_quota_exhausted' AND r.quota_until IS NOT NULL`, taskID).Scan(&source.ScheduleID, &source.Due, &source.Reset)
	return source, classify(err)
}

func quotaCoverageAllows(ctx context.Context, tx *sql.Tx, source quotaSource) (bool, error) {
	state, cursor, err := scheduleState(ctx, tx, source.ScheduleID)
	if err != nil {
		return false, err
	}
	if state == contract.StateDeleted {
		return false, nil
	}
	var blockers int64
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM occurrences o LEFT JOIN occurrence_results r ON r.task_id=o.task_id WHERE o.schedule_id=? AND o.due_unix_nanos>? AND o.due_unix_nanos<=? AND (r.task_id IS NULL OR r.verdict IN ('unknown','closed_without_outcome','committed'))`, source.ScheduleID, source.Due, cursor).Scan(&blockers)
	return blockers == 0, classify(err)
}

func (s *Store) ConsiderQuotaHold(ctx context.Context, taskID string, now time.Time) error {
	if err := task.ValidateTaskID(taskID); err != nil {
		return err
	}
	if err := contract.ValidateInstant("hold now", now); err != nil {
		return err
	}
	return s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		source, err := readQuotaSource(ctx, tx, taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		p, floor, err := readQuotaPolicy(ctx, tx, source.ScheduleID)
		if err != nil {
			return err
		}
		if !p.Enabled || source.Due <= floor || source.Reset <= now.UnixNano() {
			return nil
		}
		allowed, err := quotaCoverageAllows(ctx, tx, source)
		if err != nil || !allowed {
			return err
		}
		return installQuotaHold(ctx, tx, source, p.MaxHold, now)
	})
}

func installQuotaHold(ctx context.Context, tx *sql.Tx, source quotaSource, maxHold time.Duration, now time.Time) error {
	if _, err := tx.ExecContext(ctx, "UPDATE schedule_policy SET source_floor=? WHERE schedule_id=?", source.Due, source.ScheduleID); err != nil {
		return classify(err)
	}
	hold, err := readQuotaHold(ctx, tx, source.ScheduleID)
	if err != nil {
		return err
	}
	if hold != nil && hold.Until.After(now) {
		return nil
	}
	resetTime := contract.InstantFromUnixNanos(source.Reset)
	capEnd := now.Add(maxHold)
	untilTime := resetTime
	if capEnd.Before(resetTime) {
		untilTime = capEnd
	}
	until := untilTime.UnixNano()
	_, err = tx.ExecContext(ctx, `INSERT INTO schedule_holds(schedule_id,source_task_id,created_at,until_at,capped) VALUES (?,?,?,?,?) ON CONFLICT(schedule_id) DO UPDATE SET source_task_id=excluded.source_task_id,created_at=excluded.created_at,until_at=excluded.until_at,capped=excluded.capped`, source.ScheduleID, source.TaskID, now.UnixNano(), until, until < source.Reset)
	return classify(err)
}

func (s *Store) ResumeWithHold(ctx context.Context, id string, now time.Time) error {
	if err := task.ValidateTaskID(id); err != nil {
		return err
	}
	if err := contract.ValidateInstant("resume now", now); err != nil {
		return err
	}
	return s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		state, cursor, err := scheduleState(ctx, tx, id)
		if err != nil {
			return err
		}
		hold, err := readQuotaHold(ctx, tx, id)
		if err != nil {
			return err
		}
		span, err := contract.PlanOperationalResume(state, contract.InstantFromUnixNanos(cursor), now, hold)
		if err != nil {
			return err
		}
		if err = applyOperationalResume(ctx, tx, id, span); err != nil {
			return err
		}
		if span != nil {
			cursor = span.To.UnixNano()
		}

		if err = consumeQuotaSources(ctx, tx, id, cursor); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM schedule_holds WHERE schedule_id=?", id)
		return classify(err)
	})
}

func applyOperationalResume(ctx context.Context, tx *sql.Tx, id string, span *contract.SkipSpan) error {
	if span == nil {
		_, err := tx.ExecContext(ctx, "UPDATE schedules SET state='enabled' WHERE schedule_id=?", id)
		return classify(err)
	}
	if err := insertSkipSpan(ctx, tx, id, span.From.UnixNano(), span.From, *span); err != nil {
		return err
	}
	return updateOne(ctx, tx, "UPDATE schedules SET state='enabled',cursor_unix_nanos=? WHERE schedule_id=? AND cursor_unix_nanos=?", span.To.UnixNano(), id, span.From.UnixNano())
}

func consumeQuotaSources(ctx context.Context, tx *sql.Tx, id string, cursor int64) error {
	p, floor, err := readQuotaPolicy(ctx, tx, id)
	if err != nil {
		return err
	}
	if p.Revision == 0 {
		_, err = tx.ExecContext(ctx, "INSERT INTO schedule_policy(schedule_id,enabled,max_hold,revision,source_floor) VALUES (?,?,?,?,?)", id, false, int64(p.MaxHold), 1, max(floor, cursor))
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE schedule_policy SET source_floor=? WHERE schedule_id=?", max(floor, cursor), id)
	}
	return classify(err)
}

func quotaClaimAllowed(ctx context.Context, tx *sql.Tx, id string, due time.Time) error {
	h, err := readQuotaHold(ctx, tx, id)
	if err != nil {
		return err
	}
	if h != nil && !due.After(h.Until) {
		return fmt.Errorf("%w: schedule quota hold", contract.ErrConflict)
	}
	return nil
}

// ReconsiderQuotaHolds reads already validated advice from the ledger; it never
// reopens terminal captures merely because coverage or policy has advanced.
func (s *Store) ReconsiderQuotaHolds(ctx context.Context, id string, now time.Time) error {
	if err := task.ValidateTaskID(id); err != nil {
		return err
	}
	if err := contract.ValidateInstant("quota now", now); err != nil {
		return err
	}
	var taskID string
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT o.task_id FROM occurrences o JOIN occurrence_results r ON r.task_id=o.task_id JOIN schedule_policy p ON p.schedule_id=o.schedule_id WHERE o.schedule_id=? AND p.enabled=1 AND o.due_unix_nanos>p.source_floor AND r.verdict='rejected' AND r.code='provider_quota_exhausted' AND r.quota_until>? AND NOT EXISTS (SELECT 1 FROM occurrences later JOIN occurrence_results lr ON lr.task_id=later.task_id WHERE later.schedule_id=o.schedule_id AND later.due_unix_nanos>o.due_unix_nanos AND lr.verdict='committed') ORDER BY o.due_unix_nanos DESC LIMIT 1`, id, now.UnixNano()).Scan(&taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return classify(err)
	})
	if err != nil || taskID == "" {
		return err
	}
	return s.ConsiderQuotaHold(ctx, taskID, now)
}
