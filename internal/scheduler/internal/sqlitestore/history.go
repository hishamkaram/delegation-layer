package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// RecordSkip stores the span and advances the cursor to its end for an enabled
// schedule in one compare-and-set transaction.
func (s *Store) RecordSkip(ctx context.Context, skip contract.Skip) error {
	if err := contract.ValidateSkip(skip); err != nil {
		return err
	}
	return s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		state, cursor, err := scheduleState(ctx, tx, skip.ScheduleID)
		if err != nil {
			return err
		}
		if state != contract.StateEnabled {
			return fmt.Errorf("%w: schedule %s is %s", contract.ErrConflict, skip.ScheduleID, state)
		}
		if err := insertSkipSpan(ctx, tx, skip.ScheduleID, cursor, skip.ExpectedCursor, skip.Span); err != nil {
			return err
		}
		return updateOne(ctx, tx, `UPDATE schedules SET cursor_unix_nanos = ? WHERE schedule_id = ? AND cursor_unix_nanos = ?`,
			skip.Span.To.UnixNano(), skip.ScheduleID, cursor)
	})
}

// History returns one schedule's occurrences and skip spans together, newest
// endpoint first, below the page's exclusive Before endpoint.
func (s *Store) History(ctx context.Context, page contract.HistoryPage) (contract.HistoryPageResult, error) {
	if err := contract.ValidateHistoryPage(page); err != nil {
		return contract.HistoryPageResult{}, err
	}
	before := int64(math.MaxInt64)
	inclusive := page.Before == nil
	if !inclusive {
		before = page.Before.UnixNano()
	}
	var result contract.HistoryPageResult
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		if _, _, err := scheduleState(ctx, tx, page.ScheduleID); err != nil {
			return err
		}
		occurrences, _, err := queryOccurrences(ctx, tx, page.Limit+1, `SELECT `+occurrenceColumns+` FROM occurrences
	WHERE schedule_id = ? AND (due_unix_nanos < ? OR (? AND due_unix_nanos = ?))
	ORDER BY due_unix_nanos DESC LIMIT ?`, page.ScheduleID, before, inclusive, before, page.Limit+1)
		if err != nil {
			return err
		}
		spans, err := querySkipSpans(ctx, tx, page, before, inclusive)
		if err != nil {
			return err
		}
		result, err = mergeHistory(page, occurrences, spans)
		return err
	})
	if err != nil {
		return contract.HistoryPageResult{}, err
	}
	return result, nil
}

func querySkipSpans(ctx context.Context, tx *sql.Tx, page contract.HistoryPage, before int64, inclusive bool) ([]contract.SkipSpan, error) {
	rows, err := tx.QueryContext(ctx, `SELECT from_unix_nanos, to_unix_nanos, reason FROM skip_spans
	WHERE schedule_id = ? AND (to_unix_nanos < ? OR (? AND to_unix_nanos = ?))
	ORDER BY to_unix_nanos DESC LIMIT ?`, page.ScheduleID, before, inclusive, before, page.Limit+1)
	if err != nil {
		return nil, classify(err)
	}
	var spans []contract.SkipSpan
	err = scanEach(rows, func(rows *sql.Rows) error {
		var from, to int64
		var reason string
		if scanErr := rows.Scan(&from, &to, &reason); scanErr != nil {
			return scanErr
		}
		span := contract.SkipSpan{From: contract.InstantFromUnixNanos(from), To: contract.InstantFromUnixNanos(to), Reason: contract.SkipReason(reason)}
		if validErr := contract.ValidateSkipSpan(span); validErr != nil {
			return storedFault("skip span", validErr)
		}
		spans = append(spans, span)
		return nil
	})
	return spans, err
}

// mergeHistory merges two newest-first lists by endpoint. Strictly advancing
// cursors make every endpoint of a schedule unique, so an equal endpoint in
// both lists is an invariant fault.
func mergeHistory(page contract.HistoryPage, occurrences []contract.Occurrence, spans []contract.SkipSpan) (contract.HistoryPageResult, error) {
	var entries []contract.HistoryEntry
	for len(entries) <= page.Limit && (len(occurrences) > 0 || len(spans) > 0) {
		switch {
		case len(spans) == 0 || len(occurrences) > 0 && occurrences[0].Due.After(spans[0].To):
			entries = append(entries, contract.OccurrenceEntry(occurrences[0]))
			occurrences = occurrences[1:]
		case len(occurrences) == 0 || spans[0].To.After(occurrences[0].Due):
			entries = append(entries, contract.SkipEntry(spans[0]))
			spans = spans[1:]
		default:
			return contract.HistoryPageResult{}, fmt.Errorf("%w: schedule %s has two history entries at one endpoint",
				task.ErrInvariantFault, page.ScheduleID)
		}
	}
	result := contract.HistoryPageResult{Entries: entries, More: len(entries) > page.Limit}
	if result.More {
		result.Entries = entries[:page.Limit]
	}
	for _, entry := range result.Entries {
		if err := contract.ValidateHistoryEntry(page.ScheduleID, entry); err != nil {
			return contract.HistoryPageResult{}, storedFault("history entry", err)
		}
	}
	return result, nil
}
