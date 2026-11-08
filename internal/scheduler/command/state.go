package command

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

func PlanStateChange(current contract.Schedule, action string, now time.Time) (contract.StateChange, *Warning, error) {
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
			warning := Warning{Code: "clock_behind_cursor", Message: "Clock is behind the schedule cursor; the cursor was preserved."}
			return change, &warning, nil
		}
	}
	return change, nil, contract.ValidateStateChange(change)
}

// ApplyStateChange preserves the observed state and joined fault after an uncertain write.
func ApplyStateChange(ctx context.Context, store contract.Store, change contract.StateChange) (contract.State, error) {
	if change.From == change.To {
		return change.To, nil
	}
	if err := store.ChangeState(ctx, change); err != nil {
		observed, readErr := store.GetSchedule(ctx, change.ScheduleID)
		if readErr != nil {
			return "", errors.Join(err, readErr)
		}
		return observed.State, err
	}
	return change.To, nil
}
