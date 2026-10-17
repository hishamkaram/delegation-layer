package admission

import (
	"context"
	"encoding/json"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func acceptInspectionTerminalEventState(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, numericTaskID int64) (json.RawMessage, bool, bool, error) {
	if operation == nil || supervisor == nil || numericTaskID < 0 {
		return nil, false, false, task.ErrEvidenceFault
	}
	event, found, err := supervisor.ReadPrivateTerminalEvent(ctx, numericTaskID)
	if err != nil || !found {
		return nil, false, false, err
	}
	if event.NumericTaskID != numericTaskID {
		return nil, false, true, task.ErrEvidenceFault
	}
	// The callback is only a wake-up hint. Reconcile the exact immutable label
	// so a delayed callback cannot authorize a reused numeric ID and admission
	// never depends on a root-wide queue snapshot.
	job, current, reconcileErr := currentInspectionTerminalJob(ctx, operation, supervisor, numericTaskID)
	if reconcileErr != nil || !current {
		if facts, _, terminal, decisionErr := recordedInspectionFactsAfterObservation(ctx, operation); terminal || decisionErr != nil {
			return facts, terminal, true, decisionErr
		}
		return nil, false, true, reconcileErr
	}
	identity := operation.Request().SupervisorIdentity()
	identity.NumericTaskID = &numericTaskID
	observation := pueue.InspectionObservation{
		Binding:  supervisor.Binding(),
		Identity: identity,
		Job:      job,
		Matched:  true,
	}
	var facts json.RawMessage
	var terminal bool
	acceptErr := supervisor.CommitInspectionObservation(ctx, observation, func(observation pueue.InspectionObservation) error {
		var err error
		facts, terminal, err = acceptCompletedInspectionObservation(ctx, operation, supervisor, observation)
		return err
	})
	return facts, terminal, true, acceptErr
}

func currentInspectionTerminalJob(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, numericTaskID int64) (pueue.Job, bool, error) {
	identity := operation.Request().SupervisorIdentity()
	identity.NumericTaskID = &numericTaskID
	observation, reconcileErr := supervisor.ReconcileInspection(ctx, identity)
	retry, classifiedErr := classifyInspectionStatusObservation(observation, reconcileErr)
	if retry {
		return pueue.Job{}, false, classifiedInspectionStatusError(true, classifiedErr)
	}
	if classifiedErr != nil {
		return pueue.Job{}, false, classifiedErr
	}
	if !observation.Matched || observation.Job.ID != numericTaskID || observation.Job.State != pueue.StateEnded {
		return pueue.Job{}, false, nil
	}
	return observation.Job, true, nil
}
