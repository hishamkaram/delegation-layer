package journal

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

func (r *Resource) RecordReleaseAction(ctx context.Context, action contract.ReleaseAction) (contract.Subject, error) {
	if action.SchemaVersion != task.SchemaVersion || action.WorkspaceID != r.request.WorkspaceID || action.BudgetNanos <= 0 {
		return contract.Subject{}, task.ErrIdentityMismatch
	}
	if err := task.ValidateTaskID(action.ID); err != nil {
		return contract.Subject{}, err
	}
	data, err := task.MarshalCanonical(action)
	if err != nil {
		return contract.Subject{}, err
	}
	_, err = r.control.PutContext(ctx, "release-"+action.ID+".json", action)
	return contract.Subject{Kind: contract.SubjectOperation, ID: action.ID, SHA256: task.ComputeSHA256(data)}, err
}

func (r *Resource) RecordReleaseCapture(ctx context.Context, reservation contract.Reservation, subject contract.Subject) error {
	if subject.Kind != contract.SubjectOperation {
		return task.ErrIdentityMismatch
	}
	err := r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var action contract.ReleaseAction
		name := "release-" + subject.ID + ".json"
		if readErr := tx.Read(name, &action); readErr != nil {
			return readErr
		}
		data, marshalErr := task.MarshalCanonical(action)
		if marshalErr != nil {
			return marshalErr
		}
		if action.SchemaVersion != task.SchemaVersion || action.WorkspaceID != r.request.WorkspaceID || action.ID != subject.ID || task.ComputeSHA256(data) != subject.SHA256 {
			return task.ErrIdentityMismatch
		}
		return tx.Verify(name, action)
	})
	if err != nil {
		return err
	}
	action, err := r.ReleaseAction(ctx, subject)
	if err != nil {
		return err
	}
	return r.recordCaptureObligation(ctx, reservation, subject, nil, action.BudgetNanos)
}

func (r *Resource) ReleaseAction(ctx context.Context, subject contract.Subject) (action contract.ReleaseAction, resultErr error) {
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		name := "release-" + subject.ID + ".json"
		if err := tx.Read(name, &action); err != nil {
			return err
		}
		data, err := task.MarshalCanonical(action)
		if err != nil {
			return err
		}
		if action.SchemaVersion != task.SchemaVersion || action.WorkspaceID != r.request.WorkspaceID || action.ID != subject.ID || task.ComputeSHA256(data) != subject.SHA256 {
			return task.ErrIdentityMismatch
		}
		return tx.Verify(name, action)
	})
	return action, resultErr
}

func (r *Resource) ReadReleaseAction(ctx context.Context, id string) (action contract.ReleaseAction, resultErr error) {
	if err := task.ValidateTaskID(id); err != nil {
		return action, err
	}
	resultErr = r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		name := "release-" + id + ".json"
		if err := tx.Read(name, &action); err != nil {
			return err
		}
		if action.SchemaVersion != task.SchemaVersion || action.WorkspaceID != r.request.WorkspaceID || action.ID != id || action.BudgetNanos <= 0 {
			return task.ErrIdentityMismatch
		}
		return tx.Verify(name, action)
	})
	return action, resultErr
}

func (r *Resource) ReleaseCaptureAuthorized(ctx context.Context, obligation contract.CaptureObligation) error {
	if obligation.Predecessor != nil || obligation.Subject.Kind != contract.SubjectOperation {
		return task.ErrInvalidPermit
	}
	var action contract.ReleaseAction
	return r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		name := "release-" + obligation.Subject.ID + ".json"
		if err := tx.Read(name, &action); err != nil {
			return err
		}
		data, err := task.MarshalCanonical(action)
		if err != nil {
			return err
		}
		if action.WorkspaceID != r.request.WorkspaceID || action.ID != obligation.Subject.ID || task.ComputeSHA256(data) != obligation.Subject.SHA256 {
			return errors.Join(task.ErrIdentityMismatch, contract.ErrIdentity)
		}
		return tx.Verify(name, action)
	})
}
