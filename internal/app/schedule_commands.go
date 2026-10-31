package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

const scheduleOperationTimeout = 10 * time.Second

func scheduleCommand(a Arguments, deps storeDependencies) (result commandResult) {
	if a.ScheduleAction == "create" {
		return createScheduleCommand(a, deps)
	}
	if a.ScheduleAction == "history" || a.ScheduleAction == "execution" {
		return scheduleQueryCommand(a, deps)
	}
	response := newResponse("schedule")
	if err := validateScheduleRequest(a); err != nil {
		return failed(response, err, 2)
	}
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return failed(response, err, 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), scheduleOperationTimeout)
	defer cancel()
	store, err := openStoreContext(ctx, root, deps, false)
	if err != nil {
		if missingScheduleRoot(root, err) {
			return missingScheduleResult(a, response)
		}
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	response.RootID = store.RootID
	schedules, err := scheduler.Open(ctx, store)
	if err != nil {
		if onlyNotExist(err) {
			return missingScheduleResult(a, response)
		}
		return failed(response, err, 1)
	}
	defer func() { mergeCommandClose(&result, schedules.Close) }()
	switch a.ScheduleAction {
	case "list":
		return listSchedules(ctx, schedules, a, response)
	case "show":
		return showSchedule(ctx, schedules, a.ScheduleID, response)
	case "pause", "resume", "delete":
		return changeScheduleState(ctx, schedules, a, response, time.Now().UTC())
	default:
		return failed(response, errors.New("unknown schedule action"), 2)
	}
}

func validateScheduleRequest(a Arguments) error {
	if a.ScheduleAction == "list" {
		return contract.ValidateSchedulePage(contract.SchedulePage{AfterScheduleID: a.ScheduleAfter, Limit: a.ScheduleLimit})
	}
	if a.ScheduleAction == "history" {
		_, err := scheduleHistoryPage(a)
		return err
	}
	if a.ScheduleAction == "execution" {
		return task.ValidateTaskID(a.TaskID)
	}
	return task.ValidateTaskID(a.ScheduleID)
}

func missingScheduleRoot(root string, err error) bool {
	if !onlyNotExist(err) {
		return false
	}
	_, statErr := os.Lstat(root)
	return onlyNotExist(statErr)
}

// onlyNotExist checks every leaf, so a joined close/storage fault is retained.
func onlyNotExist(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyNotExist(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyNotExist(wrapped.Unwrap())
	}
	return errors.Is(err, fs.ErrNotExist)
}

func missingScheduleResult(a Arguments, response Response) commandResult {
	if a.ScheduleAction == "list" {
		rows := []ScheduleListRow{}
		response.Schedules = &rows
		return commandResult{response: response}
	}
	response.Schedule = &ScheduleResponse{ID: a.ScheduleID}
	return failed(response, fmt.Errorf("schedule %s: %w", a.ScheduleID, fs.ErrNotExist), 1)
}

func listSchedules(ctx context.Context, store contract.Store, a Arguments, response Response) commandResult {
	page, err := store.ListSchedules(ctx, contract.SchedulePage{AfterScheduleID: a.ScheduleAfter, Limit: a.ScheduleLimit})
	if err != nil {
		return failed(response, err, 1)
	}
	rows := make([]ScheduleListRow, 0, len(page.Schedules))
	for _, item := range page.Schedules {
		rows = append(rows, scheduleListRow(item))
	}
	response.Schedules = &rows
	if page.More && len(rows) > 0 {
		response.ScheduleCursor = rows[len(rows)-1].ID
	}
	if err := writeJSON(io.Discard, response); err != nil {
		return failed(newResponse("schedule"), err, 1)
	}
	return commandResult{response: response}
}

func showSchedule(ctx context.Context, store contract.Store, id string, response Response) commandResult {
	response.Schedule = &ScheduleResponse{ID: id}
	item, err := store.GetSchedule(ctx, id)
	if err != nil {
		return failed(response, err, 1)
	}
	projection, err := scheduleShow(item)
	if err != nil {
		return failed(response, err, 1)
	}
	response.Schedule = &projection
	if err = writeJSON(io.Discard, response); err != nil {
		response.Schedule = &ScheduleResponse{ID: id}
		return failed(response, err, 1)
	}
	if err = writeHumanResponse(io.Discard, response); err != nil {
		response.Schedule = &ScheduleResponse{ID: id}
		return failed(response, err, 1)
	}
	return commandResult{response: response}
}

func changeScheduleState(ctx context.Context, store contract.Store, a Arguments, response Response, now time.Time) commandResult {
	response.Schedule = &ScheduleResponse{ID: a.ScheduleID}
	current, err := store.GetSchedule(ctx, a.ScheduleID)
	if err != nil {
		return failed(response, err, 1)
	}
	change, warning, err := scheduleStateChange(current, a.ScheduleAction, now)
	if err != nil {
		return failed(response, err, 2)
	}
	response.Schedule.State = change.To
	if warning != nil {
		response.ScheduleWarnings = []ScheduleWarning{*warning}
	}
	if err = writeJSON(io.Discard, response); err != nil {
		return failed(response, err, 1)
	}
	if change.From == change.To {
		return commandResult{response: response}
	}
	if err = store.ChangeState(ctx, change); err != nil {
		response.Schedule.NextAction = "inspect_schedule"
		observed, readErr := store.GetSchedule(ctx, a.ScheduleID)
		if readErr == nil {
			response.Schedule.State = observed.State
		} else {
			response.Schedule.State = ""
		}
		return failed(response, errors.Join(err, readErr), 1)
	}
	return commandResult{response: response}
}

func scheduleStateChange(current contract.Schedule, action string, now time.Time) (contract.StateChange, *ScheduleWarning, error) {
	change := contract.StateChange{ScheduleID: current.Definition.ScheduleID, From: current.State}
	switch action {
	case "pause":
		change.To = contract.StatePaused
	case "resume":
		change.To = contract.StateEnabled
	case "delete":
		change.To = contract.StateDeleted
	default:
		return change, nil, errors.New("unknown schedule state action")
	}
	if change.From == change.To {
		return change, nil, nil
	}
	if change.From == contract.StatePaused && change.To == contract.StateEnabled {
		if now.After(current.Cursor) {
			change.Resume = &contract.Resume{ExpectedCursor: current.Cursor, From: current.Cursor, To: now}
		} else if now.Before(current.Cursor) {
			warning := ScheduleWarning{Code: "clock_behind_cursor", Message: "Clock is behind the schedule cursor; the cursor was preserved."}
			return change, &warning, nil
		}
	}
	return change, nil, contract.ValidateStateChange(change)
}
