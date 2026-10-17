package workspaces

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func provisionIfNeeded(ctx context.Context, _ DispatchRequest, deps OperationOptions, store *taskdir.Store, managed *Admission, budget time.Duration) (workspace.Registration, error) {
	resource := managed.resource
	registration, registrationErr := resource.Registration(ctx)
	if registrationErr != nil && !workspace.OperationAbsent(registrationErr) {
		return registration, registrationErr
	}
	attempt, err := resource.ResolveAdmissionAttempt(ctx, managed.reservation.Sequence, managed.reservation.Subject)
	if err != nil {
		return registration, err
	}
	purpose, err := attempt.ProvisionPurpose()
	if err != nil {
		return registration, err
	}
	_, purposeErr := resource.AssociatedOperation(ctx, purpose)
	if purposeErr != nil && !workspace.OperationAbsent(purposeErr) {
		return registration, purposeErr
	}
	if registrationErr == nil && workspace.OperationAbsent(purposeErr) {
		return registration, RequireProvisionHelpersClosed(ctx, store, resource)
	}
	operation, err := ScheduleOperation(ctx, store, resource, deps, OperationSpec{
		Purpose: purpose, Kind: workspace.OperationProvision, Subject: managed.reservation.Subject, Budget: budget,
	}, nil)
	if operation != nil {
		managed.operationID = operation.Request().OperationID
	}
	if err = errors.Join(err, CloseOperation(operation)); err != nil {
		return registration, err
	}
	if err = ctx.Err(); err != nil {
		return registration, errors.Join(workspace.ErrPending, err)
	}
	return awaitProvision(ctx, store, resource, managed.operationID)
}

// Registration visibility is not process closure. A reused checkout cannot
// admit a provider while any provision helper still owns its native lifetime.
func RequireProvisionHelpersClosed(ctx context.Context, store *taskdir.Store, resource *workspace.Resource) error {
	ids, err := resource.OperationIDs(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, id := range ids {
		operation, loadErr := workspace.LoadOperation(ctx, store, id)
		if loadErr != nil {
			return loadErr
		}
		var proofErr error
		if operation.Request().WorkspaceID != resource.Request().WorkspaceID {
			proofErr = task.ErrIdentityMismatch
		} else if operation.Request().Kind == workspace.OperationProvision {
			found = true
			_, proofErr = OperationClosed(ctx, operation)
		}
		if err = errors.Join(proofErr, operation.Close()); err != nil {
			return err
		}
	}
	if !found {
		return task.ErrEvidenceFault
	}
	return nil
}

func awaitProvision(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, operationID string) (registration workspace.Registration, resultErr error) {
	operation, err := workspace.LoadOperation(ctx, store, operationID)
	if err != nil {
		return registration, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	for {
		_, _, err = AcceptedOperation(ctx, store, operation)
		if err == nil {
			return resource.Registration(ctx)
		}
		if !errors.Is(err, workspace.ErrPending) {
			return registration, err
		}
		if err = WaitObservation(ctx); err != nil {
			return registration, errors.Join(workspace.ErrPending, err)
		}
	}
}

func WaitObservation(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
