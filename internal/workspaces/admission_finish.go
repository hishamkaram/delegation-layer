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
	return m.FinishContext(context.Background(), store, cause, taskCreated, recoverInspection)
}

// FinishContext preserves the caller's root admission values through the
// existing independent cleanup budget. It never renews a helper deadline.
func (m *Admission) FinishContext(parent context.Context, store *taskdir.Store, cause error, taskCreated bool, recoverInspection InspectionRecovery) AdmissionFinish {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), workspace.DefaultBudget)
	defer cancel()
	finished := AdmissionFinish{OperationID: m.operationID}
	if retained := m.retainLifetimeCleanup(store, cause); retained != nil {
		finished.CloseError = retained
		return finished
	}
	if m.resource == nil {
		finished.CloseError = m.Close()
		return finished
	}
	if cause != nil && !m.existing && m.reservation.Sequence != 0 && !taskCreated {
		abortErr := ReconcileUnlaunchedAdmission(ctx, store, m.resource, m.reservation, recoverInspection)
		if retained := m.retainLifetimeCleanup(store, abortErr); retained != nil {
			finished.AbortError, finished.CloseError = abortErr, retained
			return finished
		}
		if abortErr != nil && (!errors.Is(abortErr, workspace.ErrPending) || !PendingError(abortErr)) {
			finished.AbortError = abortErr
			cause = errors.Join(cause, abortErr)
		}
	}
	finished.Description, finished.DescriptionError, finished.CloseError = m.describeFinish(ctx, store, cause)
	if finished.CloseError == nil {
		finished.CloseError = m.Close()
	}
	return finished
}

func (m *Admission) describeFinish(ctx context.Context, store *taskdir.Store, cause error) (Description, error, error) {
	description, err := Describe(ctx, m.resource)
	if retained := m.retainLifetimeCleanup(store, err); retained != nil {
		return description, err, retained
	}
	if m.operationID == "" {
		return description, err, nil
	}
	description.OperationID = m.operationID
	operationErr := DescribeOperation(ctx, store, m.resource, m.operationID, &description)
	if retained := m.retainLifetimeCleanup(store, operationErr); retained != nil {
		return description, errors.Join(err, operationErr), retained
	}
	if operationErr != nil && (!errors.Is(operationErr, workspace.ErrPending) || !PendingError(operationErr)) && cause == nil {
		err = errors.Join(err, operationErr)
	}
	return description, err, nil
}
