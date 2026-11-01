package app

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

func scheduleHistoryPage(a Arguments) (contract.HistoryPage, error) {
	page := contract.HistoryPage{ScheduleID: a.ScheduleID, Limit: a.ScheduleLimit}
	if a.ScheduleBefore != "" {
		before, err := time.Parse(time.RFC3339Nano, a.ScheduleBefore)
		if err != nil {
			return page, fmt.Errorf("history --before must be RFC3339Nano: %w", err)
		}
		before = before.UTC()
		page.Before = &before
	}
	return page, contract.ValidateHistoryPage(page)
}

func scheduleHistory(ctx context.Context, store contract.Store, a Arguments, response Response) commandResult {
	query, err := scheduleHistoryPage(a)
	if err != nil {
		return failed(response, err, 2)
	}
	response.ScheduleHistory = &ScheduleHistoryResponse{ScheduleID: a.ScheduleID}
	page, err := store.History(ctx, query)
	if err != nil {
		return failedScheduleQuery(response, err)
	}
	response.ScheduleHistory.Entries = make([]ScheduleHistoryEntry, 0, len(page.Entries))
	for _, entry := range page.Entries {
		row, err := scheduleHistoryEntry(a.ScheduleID, entry)
		if err != nil {
			return failedScheduleQuery(response, err)
		}
		response.ScheduleHistory.Entries = append(response.ScheduleHistory.Entries, row)
	}
	if next, more, err := page.Next(query); err != nil {
		return failedScheduleQuery(response, err)
	} else if more {
		response.ScheduleHistory.NextBefore = next.Before.Format(time.RFC3339Nano)
	}
	if err := writeJSON(io.Discard, response); err != nil {
		response.ScheduleHistory.Entries = nil
		response.ScheduleHistory.NextBefore = ""
		return failedScheduleQuery(response, err)
	}
	return commandResult{response: response}
}
