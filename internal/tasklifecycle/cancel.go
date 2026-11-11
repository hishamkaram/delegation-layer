package tasklifecycle

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/stop"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func Cancel(root string, td *taskdir.TaskDir, options pueue.Options, lookup installation.ExecutableLookup, openControl stop.ControlClientOpener) (result Cancellation, resultErr error) {
	return CancelWithRequestID(root, td, "", options, lookup, openControl)
}

// CancelWithRequestID preserves the originating cancellation identity across transport loss.
func CancelWithRequestID(root string, td *taskdir.TaskDir, requestID string, options pueue.Options, lookup installation.ExecutableLookup, openControl stop.ControlClientOpener) (result Cancellation, resultErr error) {
	return CancelWithRequestIDContext(context.Background(), root, td, requestID, options, lookup, openControl)
}

// CancelWithRequestIDContext retains the caller operation bound without changing task budget.
func CancelWithRequestIDContext(ctx context.Context, root string, td *taskdir.TaskDir, requestID string, options pueue.Options, lookup installation.ExecutableLookup, openControl stop.ControlClientOpener) (result Cancellation, resultErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	inspection, err := td.InspectContext(ctx)
	result.Inspection = inspection
	if err != nil {
		return result, err
	}
	if inspection.Terminal() {
		return result, nil
	}
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return result, err
	}
	submission, err := td.ReadSubmission()
	if err == nil {
		if !task.SameSupervisorIdentity(submission.Supervisor, meta.SupervisorConfig) {
			return result, task.ErrIdentityMismatch
		}
	} else if !task.RecordAbsent(err) {
		result.Failure = CancellationFailureSubmissionRead
		return result, err
	}
	// Persist the stop request first. The active lifetime owner handles native
	// termination; only an idle task lets this command stop the saved queue row.
	requestID, err = cancellationRequestID(requestID)
	if err != nil {
		return result, err
	}

	outcome, stopErr := stop.Task(ctx, root, td, requestID, "user", options, lookup, openControl)
	if errors.Is(stopErr, task.ErrTerminalTask) {
		return terminalAfterCancelRace(ctx, result, td)
	}
	if errors.Is(stopErr, task.ErrStopAlreadyRequested) {
		if duplicateCancellationOnly(stopErr) {
			stopErr = nil
		}
	} else {
		result.Stop = &outcome
	}
	if records, recordsErr := td.ReadStopRecords(); recordsErr == nil {
		result.Stops, result.StopsRead = records, true
	} else {
		stopErr = errors.Join(stopErr, recordsErr)
	}
	finalInspection, inspectErr := td.InspectContext(ctx)
	if finalInspection != nil {
		result.Inspection = finalInspection
	}
	return result, errors.Join(stopErr, inspectErr)
}

func terminalAfterCancelRace(ctx context.Context, result Cancellation, td *taskdir.TaskDir) (Cancellation, error) {
	inspection, err := td.InspectContext(ctx)
	if inspection != nil {
		result.Inspection = inspection
	}
	if err != nil {
		return result, err
	}
	if inspection.Terminal() {
		return result, nil
	}
	return result, task.ErrTerminalTask
}

// A duplicate intent is an idempotent acknowledgment only when no independent
// storage, durability, or cleanup fault accompanies it.
func duplicateCancellationOnly(err error) bool {
	return task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, task.ErrStopAlreadyRequested) })
}

func cancellationRequestID(requestID string) (string, error) {
	if requestID == "" {
		return randomid.NewRandomID()
	}
	return requestID, task.ValidateTaskID(requestID)
}
