package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/recurrence"
)

func BuildDefinition(input CreateInput) (contract.Definition, []Warning, error) {
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
		return contract.Definition{}, nil, InvalidInput(err)
	}
	var warnings []Warning
	for _, warning := range recurrence.Warnings(input.Cron, zone) {
		warnings = append(warnings, Warning{Code: warning.Code, Message: warning.Message})
	}
	return definition, warnings, nil
}

// Create preserves uncertain-write readback of the exact captured definition.
// The boolean reports a successful match after an uncertain create only.
func Create(ctx context.Context, store contract.Store, definition contract.Definition) (bool, error) {
	if err := store.CreateSchedule(ctx, definition); err != nil {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), OperationTimeout)
		defer cancel()
		match, readErr := readBackDefinition(readCtx, store, definition)
		return match, fmt.Errorf("schedule create outcome requires inspection: %w", errors.Join(err, readErr))
	}
	return false, nil
}

func readBackDefinition(ctx context.Context, store contract.Store, expected contract.Definition) (bool, error) {
	actual, err := store.GetSchedule(ctx, expected.ScheduleID)
	if err != nil {
		return false, err
	}
	want, err := contract.DefinitionDigests(expected)
	if err != nil {
		return false, err
	}
	got, err := contract.DefinitionDigests(actual.Definition)
	if err != nil {
		return false, err
	}
	if got != want {
		return false, fmt.Errorf("schedule %s: %w", expected.ScheduleID, contract.ErrConflict)
	}
	return true, nil
}
