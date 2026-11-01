package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/host"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// checkSchedulerDefinitions runs before resident readiness. A stored definition
// this build cannot read is an explicit incompatibility, never a crash loop.
func checkSchedulerDefinitions(ctx context.Context, schedules contract.Store) error {
	page := contract.SchedulePage{Limit: contract.MaxPageLimit}
	for ctx.Err() == nil {
		result, err := schedules.ListSchedules(ctx, page)
		if err != nil {
			return schedulerDefinitionReadError("reading saved scheduler definitions", err)
		}
		for _, summary := range result.Schedules {
			id := summary.Header.ScheduleID
			if id <= page.AfterScheduleID {
				return fmt.Errorf("%w: unordered definition page", host.ErrIncompatible)
			}
			page.AfterScheduleID = id
			if _, err = schedules.GetSchedule(ctx, id); err != nil {
				return schedulerDefinitionReadError("reading saved schedule "+id, err)
			}
		}
		if !result.More {
			return nil
		}
		if len(result.Schedules) == 0 {
			return fmt.Errorf("%w: empty definition continuation page", host.ErrIncompatible)
		}
	}
	return ctx.Err()
}

func schedulerDefinitionReadError(subject string, err error) error {
	if errors.Is(err, task.ErrInvariantFault) || errors.Is(err, task.ErrUnsupportedSchema) || errors.Is(err, task.ErrEvidenceFault) {
		return fmt.Errorf("%w: %s: %w", host.ErrIncompatible, subject, err)
	}
	return fmt.Errorf("%s: %w", subject, err)
}
