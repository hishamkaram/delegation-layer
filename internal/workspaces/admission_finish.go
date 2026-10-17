package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// Finish retains the use owner through admission recovery and observation,
// then releases it once. The caller presents errors in their original phases.
func (m *Admission) Finish(store *taskdir.Store, cause error, taskCreated bool, recoverInspection InspectionRecovery) AdmissionFinish {
	ctx, cancel := context.WithTimeout(context.Background(), workspace.DefaultBudget)
	defer cancel()
	finished := AdmissionFinish{OperationID: m.operationID}
	if cause != nil && !m.existing && m.reservation.Sequence != 0 && !taskCreated {
		abortErr := ReconcileUnlaunchedAdmission(ctx, store, m.resource, m.reservation, recoverInspection)
		if abortErr != nil && !errors.Is(abortErr, workspace.ErrPending) {
			finished.AbortError = abortErr
			cause = errors.Join(cause, abortErr)
		}
	}
	description, err := Describe(ctx, m.resource)
	if m.operationID != "" {
		description.OperationID = m.operationID
		operationErr := DescribeOperation(ctx, store, m.resource, m.operationID, &description)
		if operationErr != nil && !errors.Is(operationErr, workspace.ErrPending) && cause == nil {
			err = errors.Join(err, operationErr)
		}
	}
	finished.Description, finished.DescriptionError = description, err
	finished.CloseError = m.Close()
	return finished
}
