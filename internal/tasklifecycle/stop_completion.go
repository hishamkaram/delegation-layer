package tasklifecycle

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func releaseLifetimeOwnerAfterPending(pending <-chan struct{}, owner guardian.Owner) error {
	if owner == nil {
		return task.ErrEvidenceFault
	}
	if pending != nil {
		// The pending supervisor mutation can still stop the managed guardian.
		// Releasing its lifetime lease first would let a new guardian launch
		// while that older mutation remains capable of ending the queue row.
		<-pending
	}
	return owner.Release()
}

func recordStoppedTaskTermination(ctx context.Context, td *taskdir.TaskDir, requestID string, result pueue.StopResult) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	if td == nil {
		return false, task.ErrEvidenceFault
	}
	if !result.Matched || result.ObservedState != pueue.StateEnded || result.NumericTaskID == nil {
		return false, nil
	}
	if err := guardian.RecoverTaskLifetime(ctx, td); errors.Is(err, guardian.ErrRecoveryPending) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	ready, err := td.StopTerminationReadyContext(ctx)
	if err != nil || !ready {
		return false, err
	}
	err = td.RecordStopObservationContext(ctx, requestID, task.StopObservationFacts{NumericTaskID: *result.NumericTaskID, State: "ended", Terminated: true})
	return err == nil, err
}
