package stop

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Task uses the existing durable stop owner under a bounded operation.
func Task(ctx context.Context, root string, td *taskdir.TaskDir, requestID, cause string, supervisorOptions pueue.Options, lookup installation.ExecutableLookup, openControl ControlClientOpener) (response Outcome, resultErr error) {
	if err := ctx.Err(); err != nil {
		return response, err
	}
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return Outcome{}, err
	}
	supervisorOptions = supervisorcore.OptionsWithEnvironment(supervisorOptions, meta.Environment)
	permit, err := td.PrepareStopContext(ctx, requestID, cause, time.Time{})
	if err != nil {
		return Outcome{}, err
	}
	request, err := permit.Request()
	if err != nil {
		return Outcome{}, errors.Join(err, permit.Release())
	}
	owner, err := td.TryAcquireLifetimeOwnerContext(ctx)
	if errors.Is(err, task.ErrLockBusy) {
		consumeErr := permit.Consume()
		releaseErr := permit.Release()
		return Outcome{
			requestID:     requestID,
			cause:         cause,
			requested:     true,
			attempted:     consumeErr == nil,
			action:        "terminate",
			numericTaskID: copyTaskID(request.NumericTaskID),
			inFlight:      consumeErr == nil,
			observedState: string(pueue.StateUnknown),
			message:       "durable cancellation routed to the task lifetime owner",
		}, errors.Join(consumeErr, releaseErr)
	}
	if err != nil {
		return Outcome{}, errors.Join(err, permit.Release())
	}
	ownerHeld := true
	defer func() {
		if ownerHeld {
			// LifetimeOwnerLease release is deliberately retryable. A failed
			// unlock keeps the lease authoritative, so the deferred attempt must
			// run instead of abandoning the native-lifetime fence.
			resultErr = errors.Join(resultErr, owner.Release())
		}
	}()
	if request.NumericTaskID == nil {
		return stopTaskWithoutNumericTargetContext(ctx, td, permit, requestID, cause)
	}
	source, err := supervisorcore.NewSavedBindingSource(meta.RootID, infrastructure.SavedBindingTaskStop, td.TaskID, requestID, request.Supervisor)
	if err != nil {
		return Outcome{}, errors.Join(err, permit.Release())
	}
	client, err := openControl(ctx, root, request.Supervisor, source, supervisorOptions, lookup)
	if err != nil {
		return Outcome{}, errors.Join(supervisorcore.ClosePrivateAdmissionTransition(client, err), permit.Release())
	}
	defer func() { supervisorcore.CloseMaintenance(client, resultErr) }()
	stopResult, stopErr := client.Stop(pueue.WithControlLease(ctx, owner), permit)
	stopErr = errors.Join(stopErr, permit.Release())
	if stopResult.Acknowledged != nil && stopResult.NumericTaskID != nil {
		stopErr = errors.Join(stopErr, td.RecordStopReplyContext(ctx, requestID, task.StopReplyFacts{NumericTaskID: *stopResult.NumericTaskID, Action: stopResult.Action, Acknowledged: *stopResult.Acknowledged, Message: stopResult.Message}))
	}
	var pendingStop <-chan struct{}
	if stopResult.Pending != nil {
		pendingStop = stopResult.Pending.Done()
	}
	if releaseErr := releaseLifetimeOwnerAfterPending(pendingStop, owner); releaseErr != nil {
		stopErr = errors.Join(stopErr, releaseErr)
	} else {
		ownerHeld = false
	}
	terminated, terminationErr := recordStoppedTaskTermination(ctx, td, requestID, stopResult)
	stopErr = errors.Join(stopErr, terminationErr)
	response = stopOutcomeFromResult(requestID, cause, stopResult)
	response.terminated = terminated
	return response, stopErr
}

// A missing numeric receipt is not authority to control any queue row or to
// restart its supervisor. The durable user stop fences later task launch; only
// the existing lifetime/run owners can additionally prove never-launched.
func stopTaskWithoutNumericTargetContext(ctx context.Context, td *taskdir.TaskDir, permit *taskdir.StopPermit, requestID, cause string) (Outcome, error) {
	proven, err := td.ProveUnlaunchedTaskOwnedContext(ctx)
	if err != nil {
		return Outcome{}, errors.Join(err, permit.Release())
	}
	response := Outcome{
		requestID: requestID, cause: cause, requested: true, action: "terminate",
		observedState: string(pueue.StateUnknown), message: "durable cancellation recorded; native termination is not yet proven",
	}
	if proven {
		response.action, response.terminated = "not-launched", true
		response.message = "durable cancellation prevents this unlaunched task from starting"
	}
	return response, errors.Join(permit.Consume(), permit.Release())
}
