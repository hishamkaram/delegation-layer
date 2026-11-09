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

func (s *Store) ClaimNotification(ctx context.Context, id, eventID string, now time.Time) (*contract.Notification, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	if err := task.ValidateTaskID(eventID); err != nil {
		return nil, err
	}
	if err := contract.ValidateInstant("notification now", now); err != nil {
		return nil, err
	}
	var n *contract.Notification
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		n, err = selectNotification(ctx, tx, id, now)
		if err != nil || n == nil {
			return err
		}
		n.EventID = eventID
		n.CreatedAt = now
		n.Result = "claimed"
		_, err = tx.ExecContext(ctx, `INSERT INTO notification_attempts(event_id,schedule_id,kind,anchor_task_id,last_task_id,streak_count,failure_code,created_at,result) VALUES (?,?,?,?,?,?,?,?,'claimed')`, eventID, id, n.Kind, n.AnchorTaskID, n.LastTaskID, n.StreakCount, n.FailureCode, now.UnixNano())
		return classify(err)
	})
	return n, err
}

func selectNotification(ctx context.Context, tx *sql.Tx, id string, now time.Time) (*contract.Notification, error) {
	var row scheduleRow
	var length int64
	err := tx.QueryRowContext(ctx, `SELECT `+scheduleColumns+`,length(brief) FROM schedules WHERE schedule_id=?`, id).Scan(append(row.targets(), &length)...)
	if err != nil {
		return nil, classify(err)
	}
	summary, err := row.summary(length)
	if err != nil {
		return nil, err
	}
	if summary.Header.Hook == nil {
		return nil, nil
	}
	h, err := readHealth(ctx, tx, id, now)
	if err != nil {
		return nil, err
	}
	if h.LastOccurrence == nil {
		return nil, nil
	}
	if h.ConsecutiveNonCommitted >= summary.Header.Hook.Threshold {
		return failureNotification(ctx, tx, h)
	}
	if h.LastCommitted != nil && h.LastOccurrence.TaskID == h.LastCommitted.TaskID {
		return recoveryNotification(ctx, tx, h)
	}
	return nil, nil
}

func failureNotification(ctx context.Context, tx *sql.Tx, h contract.Health) (*contract.Notification, error) {
	boundary, err := streakBoundary(ctx, tx, h.ScheduleID)
	if err != nil {
		return nil, err
	}
	var claimed int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM notification_attempts n JOIN occurrences o ON n.anchor_task_id=o.task_id WHERE n.schedule_id=? AND n.kind='failure' AND o.due_unix_nanos>?`, h.ScheduleID, boundary).Scan(&claimed)
	if err != nil {
		return nil, classify(err)
	}
	if claimed > 0 {
		return nil, nil
	}
	return &contract.Notification{ScheduleID: h.ScheduleID, Kind: "failure", AnchorTaskID: h.LastOccurrence.TaskID, LastTaskID: h.LastOccurrence.TaskID, StreakCount: h.ConsecutiveNonCommitted, FailureCode: h.DominantFailureCode}, nil
}

func recoveryNotification(ctx context.Context, tx *sql.Tx, h contract.Health) (*contract.Notification, error) {
	prior, err := readNotification(ctx, tx, `WHERE schedule_id=? AND kind='failure' ORDER BY created_at DESC,event_id DESC LIMIT 1`, h.ScheduleID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recovered int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM notification_attempts WHERE schedule_id=? AND kind='recovery' AND anchor_task_id=?`, h.ScheduleID, prior.AnchorTaskID).Scan(&recovered)
	if err != nil {
		return nil, classify(err)
	}
	if recovered > 0 {
		return nil, nil
	}
	var anchorDue int64
	if err = tx.QueryRowContext(ctx, "SELECT due_unix_nanos FROM occurrences WHERE task_id=?", prior.AnchorTaskID).Scan(&anchorDue); err != nil {
		return nil, classify(err)
	}
	if !h.LastCommitted.Due.After(contract.InstantFromUnixNanos(anchorDue)) {
		return nil, nil
	}
	return &contract.Notification{ScheduleID: h.ScheduleID, Kind: "recovery", AnchorTaskID: prior.AnchorTaskID, LastTaskID: h.LastOccurrence.TaskID}, nil
}

func (s *Store) FinishNotification(ctx context.Context, eventID, result string) error {
	if err := task.ValidateTaskID(eventID); err != nil {
		return err
	}
	if err := contract.ValidateNotificationResult(result); err != nil {
		return err
	}
	return s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		return updateOne(ctx, tx, "UPDATE notification_attempts SET result=? WHERE event_id=? AND result='claimed'", result, eventID)
	})
}

func readNotification(ctx context.Context, tx *sql.Tx, suffix string, args ...any) (*contract.Notification, error) {
	var n contract.Notification
	var created int64
	err := tx.QueryRowContext(ctx, `SELECT event_id,schedule_id,kind,anchor_task_id,last_task_id,streak_count,failure_code,created_at,result FROM notification_attempts `+suffix, args...).Scan(&n.EventID, &n.ScheduleID, &n.Kind, &n.AnchorTaskID, &n.LastTaskID, &n.StreakCount, &n.FailureCode, &created, &n.Result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, classify(err)
	}
	n.CreatedAt = contract.InstantFromUnixNanos(created)
	return &n, nil
}
