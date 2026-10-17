package tasklifecycle

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/guardian"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func UpdateStatusStops(ctx context.Context, td *taskdir.TaskDir, response *Observation, observation pueue.Observation) error {
	if ctx == nil {
		return context.Canceled
	}
	response.LivenessObserved = true
	queueEnded := observation.State == pueue.StateEnded && observation.Matched
	if queueEnded {
		response.Liveness = task.LivenessRunning
	} else {
		response.Liveness = LivenessFromQueue(observation.State)
	}
	records, err := td.ReadStopRecords()
	if err != nil {
		return err
	}
	if !queueEnded {
		response.Stops, response.StopsRead = records, true
		return nil
	}
	if err = guardian.RecoverTaskLifetime(ctx, td); errors.Is(err, guardian.ErrRecoveryPending) {
		response.Liveness = task.LivenessRunning
		response.Pending = PendingLifetimeFinalization
		response.Stops, response.StopsRead = records, true
		return nil
	} else if err != nil {
		return err
	}
	ready, err := td.StopTerminationReadyContext(ctx)
	if err != nil {
		return err
	}
	if !ready {
		response.Stops, response.StopsRead = records, true
		return nil
	}
	response.Liveness = task.LivenessEnded
	var recordErr error
	for _, record := range records {
		if record.Request == nil || record.Request.NumericTaskID == nil || *record.Request.NumericTaskID != observation.NumericTaskID {
			continue
		}
		recordErr = errors.Join(recordErr, td.RecordStopObservationContext(ctx, record.Request.RequestID, task.StopObservationFacts{NumericTaskID: observation.NumericTaskID, State: "ended", Terminated: true}))
	}
	refreshed, refreshErr := td.ReadStopRecords()
	if refreshErr == nil {
		response.Stops, response.StopsRead = refreshed, true
	}
	return errors.Join(recordErr, refreshErr)
}
