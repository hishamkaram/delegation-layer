package sqlitestore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var _ contract.ReliabilityStore = (*Store)(nil)

func (s *Store) RecordAttempt(ctx context.Context, id string, f contract.AttemptFailure) error {
	if err := task.ValidateTaskID(id); err != nil {
		return err
	}
	if err := contract.ValidateAttempt(f); err != nil {
		return err
	}
	encoded, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		var prior []byte
		err := tx.QueryRowContext(ctx, "SELECT attempt_json FROM occurrence_attempts WHERE task_id = ?", id).Scan(&prior)
		if err == nil {
			if !bytes.Equal(prior, encoded) {
				return storedFault("attempt", errors.New("immutable attempt changed"))
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return classify(err)
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO occurrence_attempts(task_id,attempt_json) VALUES (?,?)", id, encoded)
		return classify(err)
	})
}

func (s *Store) RecordResult(ctx context.Context, f contract.ResultFact) error {
	if err := contract.ValidateResultFact(f); err != nil {
		return err
	}
	return s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		var verdict string
		var reference sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT verdict,outcome_sha256 FROM occurrence_results WHERE task_id = ?", f.TaskID).Scan(&verdict, &reference)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return classify(err)
		}
		if verdict != "" && verdict != "unknown" && f.Verdict == "unknown" {
			return nil
		}
		if verdict == "committed" || verdict == "rejected" {
			if f.Verdict == "unknown" || f.Verdict == "closed_without_outcome" {
				return nil
			}
			if verdict != f.Verdict || reference.String != f.OutcomeSHA256 {
				return storedFault("result", errors.New("terminal outcome projection changed"))
			}
			return nil
		}
		var quota sql.NullInt64
		if !f.QuotaNotBefore.IsZero() {
			quota = sql.NullInt64{Int64: f.QuotaNotBefore.UnixNano(), Valid: true}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO occurrence_results(task_id,observed_at,verdict,code,outcome_sha256,quota_until) VALUES (?,?,?,?,?,?)
 ON CONFLICT(task_id) DO UPDATE SET observed_at=excluded.observed_at,verdict=excluded.verdict,code=excluded.code,outcome_sha256=excluded.outcome_sha256,quota_until=excluded.quota_until WHERE excluded.observed_at > occurrence_results.observed_at`, f.TaskID, f.ObservedAt.UnixNano(), f.Verdict, nullableText(f.Code), nullableText(f.OutcomeSHA256), quota)
		return classify(err)
	})
}

// ReconciliationPage pages immutable occurrence identities, not just active
// reservations. Repeating rounds revisits late publications without launching.
func (s *Store) ReconciliationPage(ctx context.Context, scheduleID, after string, limit int) ([]contract.Occurrence, bool, error) {
	if scheduleID != "" {
		if err := task.ValidateTaskID(scheduleID); err != nil {
			return nil, false, err
		}
	}
	if after != "" {
		if err := task.ValidateTaskID(after); err != nil {
			return nil, false, err
		}
	}
	if limit < 1 || limit > contract.MaxPageLimit {
		return nil, false, errors.New("invalid reconciliation page limit")
	}
	var occurrences []contract.Occurrence
	var more bool
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		occurrences, more, err = queryOccurrences(ctx, tx, limit, `SELECT `+occurrenceColumns+` FROM occurrences WHERE task_id > ? AND (? = '' OR schedule_id = ?) AND NOT EXISTS (SELECT 1 FROM occurrence_results r WHERE r.task_id=occurrences.task_id AND r.verdict IN ('committed','rejected','not_created')) ORDER BY task_id LIMIT ?`, after, scheduleID, scheduleID, limit+1)
		return err
	})
	return occurrences, more, err
}
