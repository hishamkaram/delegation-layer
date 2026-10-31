package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/recurrence"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

func createScheduleCommand(a Arguments, deps storeDependencies) (result commandResult) {
	response := newResponse("schedule")
	input, err := prepareScheduleCreate(a, time.Now, randomid.NewTaskID)
	if err != nil {
		code := 1
		if errors.Is(err, errScheduleInput) {
			code = 2
		}
		return failed(response, err, code)
	}
	response.Schedule = &ScheduleResponse{ID: input.ID}
	definition, warnings, err := scheduleDefinition(input)
	if err != nil {
		code := 1
		if !errors.Is(err, recurrence.ErrLookup) && !errors.Is(err, recurrence.ErrRangeExhausted) && (errors.Is(err, recurrence.ErrInvalidExpression) || errors.Is(err, recurrence.ErrInvalidTimezone) || errors.Is(err, recurrence.ErrLocalTimezone) || errors.Is(err, errScheduleInput)) {
			code = 2
		}
		return failed(response, err, code)
	}
	response.ScheduleWarnings = warnings
	response.Schedule.State = contract.StateEnabled
	if err = writeJSON(io.Discard, response); err != nil {
		return failed(response, err, 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), scheduleOperationTimeout)
	defer cancel()
	root, err := openStoreContext(ctx, input.Root, deps, true)
	if err != nil {
		response.Schedule.State = ""
		return failed(response, err, 1)
	}
	defer func() { mergeScheduleClose(&result, root.Close) }()
	response.RootID = root.RootID
	store, err := openScheduleForCreate(ctx, root)
	if err != nil {
		response.Schedule.State = ""
		return failed(response, err, 1)
	}
	defer func() { mergeScheduleClose(&result, store.Close) }()
	if err = writeJSON(io.Discard, response); err != nil {
		response.Schedule.State = ""
		return failed(response, err, 1)
	}
	return createSchedule(ctx, store, definition, response)
}

func scheduleDefinition(input scheduleCreateInput) (contract.Definition, []ScheduleWarning, error) {
	zone, err := recurrence.ResolveTimezone(input.Timezone)
	if err != nil {
		return contract.Definition{}, nil, err
	}
	if _, err = recurrence.Next(input.Cron, zone, input.CreatedAt); err != nil {
		return contract.Definition{}, nil, err
	}
	definition := contract.Definition{DefinitionHeader: contract.DefinitionHeader{
		ScheduleID: input.ID, CreatedAt: input.CreatedAt, Cron: input.Cron, Timezone: zone, Request: input.Request,
	}, Brief: input.Brief}
	if err = contract.ValidateDefinition(definition); err != nil {
		return contract.Definition{}, nil, invalidScheduleInput(err)
	}
	var warnings []ScheduleWarning
	for _, warning := range recurrence.Warnings(input.Cron, zone) {
		warnings = append(warnings, ScheduleWarning{Code: warning.Code, Message: warning.Message})
	}
	return definition, warnings, nil
}

func createSchedule(ctx context.Context, store contract.Store, definition contract.Definition, response Response) commandResult {
	if err := store.CreateSchedule(ctx, definition); err != nil {
		response.Schedule.State = ""
		response.Schedule.NextAction = "inspect_schedule"
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduleOperationTimeout)
		defer cancel()
		match, readErr := readBackScheduleDefinition(readCtx, store, definition)
		response.Schedule.DefinitionMatched = match
		return failed(response, fmt.Errorf("schedule create outcome requires inspection: %w", errors.Join(err, readErr)), 1)
	}
	return commandResult{response: response}
}

func mergeScheduleClose(result *commandResult, closeFn func() error) {
	mergeCommandClose(result, closeFn)
	if result.code == 1 && result.response.Schedule != nil && (result.response.Schedule.State == contract.StateEnabled || result.response.Schedule.NextAction == "inspect_schedule") {
		result.response.Schedule.NextAction = "inspect_schedule"
	}
}
