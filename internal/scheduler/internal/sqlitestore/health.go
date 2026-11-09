package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (s *Store) Health(ctx context.Context, id string, now time.Time) (contract.Health, error) {
	if err := contract.ValidateInstant("health now", now); err != nil {
		return contract.Health{}, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return contract.Health{}, err
	}
	var h contract.Health
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		h, err = readHealth(ctx, tx, id, now)
		return err
	})
	return h, err
}

func readHealth(ctx context.Context, tx *sql.Tx, id string, now time.Time) (contract.Health, error) {
	h := contract.Health{ScheduleID: id}
	if _, _, err := scheduleState(ctx, tx, id); err != nil {
		return h, err
	}
	var err error
	h.LastOccurrence, err = readHealthOccurrence(ctx, tx, id, false)
	if err != nil {
		return h, err
	}
	h.LastCommitted, err = readHealthOccurrence(ctx, tx, id, true)
	if err != nil {
		return h, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM occurrences o LEFT JOIN occurrence_results r ON r.task_id=o.task_id WHERE o.schedule_id=? AND (r.task_id IS NULL OR r.verdict='unknown')`, id).Scan(&h.Unresolved); err != nil {
		return h, classify(err)
	}
	h.Complete = h.Unresolved == 0
	boundary, err := streakBoundary(ctx, tx, id)
	if err != nil {
		return h, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM occurrences o JOIN occurrence_results r ON r.task_id=o.task_id WHERE o.schedule_id=? AND o.due_unix_nanos>? AND r.verdict NOT IN ('unknown','committed')`, id, boundary).Scan(&h.ConsecutiveNonCommitted); err != nil {
		return h, classify(err)
	}
	err = tx.QueryRowContext(ctx, `SELECT coalesce(r.code,'result_not_committed') FROM occurrences o JOIN occurrence_results r ON r.task_id=o.task_id WHERE o.schedule_id=? AND o.due_unix_nanos>? AND r.verdict NOT IN ('unknown','committed') GROUP BY coalesce(r.code,'result_not_committed') ORDER BY count(*) DESC,coalesce(r.code,'result_not_committed') ASC LIMIT 1`, id, boundary).Scan(&h.DominantFailureCode)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return h, classify(err)
	}
	h.Hold, h.HoldDeferred, err = readHoldHealth(ctx, tx, id, now)
	if err != nil {
		return h, err
	}

	notification, err := readNotification(ctx, tx, `WHERE schedule_id=? ORDER BY created_at DESC,event_id DESC LIMIT 1`, id)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return h, err
	}
	h.LastNotification = notification
	return h, nil
}

func readHoldHealth(ctx context.Context, tx *sql.Tx, id string, now time.Time) (*contract.QuotaHold, bool, error) {
	hold, err := readQuotaHold(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	if hold != nil && !hold.Until.After(now) {
		hold = nil
	}
	p, floor, err := readQuotaPolicy(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	if !p.Enabled {
		return hold, false, nil
	}
	var n int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM occurrence_results r JOIN occurrences o ON r.task_id=o.task_id WHERE o.schedule_id=? AND o.due_unix_nanos>? AND r.quota_until>? AND NOT EXISTS (SELECT 1 FROM occurrences later JOIN occurrence_results lr ON lr.task_id=later.task_id WHERE later.schedule_id=o.schedule_id AND later.due_unix_nanos>o.due_unix_nanos AND lr.verdict='committed')`, id, floor, now.UnixNano()).Scan(&n)
	return hold, n > 0, classify(err)
}

func streakBoundary(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	var boundary sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT max(o.due_unix_nanos) FROM occurrences o LEFT JOIN occurrence_results r ON r.task_id=o.task_id WHERE o.schedule_id=? AND (r.task_id IS NULL OR r.verdict IN ('unknown','committed'))`, id).Scan(&boundary)
	if err != nil {
		return 0, classify(err)
	}
	if !boundary.Valid {
		return -9223372036854775808, nil
	}
	return boundary.Int64, nil
}

func readHealthOccurrence(ctx context.Context, tx *sql.Tx, id string, committed bool) (*contract.HealthOccurrence, error) {
	query := `SELECT o.task_id,o.due_unix_nanos,r.observed_at FROM occurrences o LEFT JOIN occurrence_results r ON r.task_id=o.task_id WHERE o.schedule_id=?`
	if committed {
		query += ` AND r.verdict='committed'`
	}
	query += ` ORDER BY o.due_unix_nanos DESC LIMIT 1`
	var h contract.HealthOccurrence
	var due int64
	var observed sql.NullInt64
	err := tx.QueryRowContext(ctx, query, id).Scan(&h.TaskID, &due, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, classify(err)
	}
	h.Due = contract.InstantFromUnixNanos(due)
	if observed.Valid {
		v := contract.InstantFromUnixNanos(observed.Int64)
		h.ObservedAt = &v
	}
	return &h, nil
}
