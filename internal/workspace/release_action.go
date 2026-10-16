package workspace

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// ReleaseAction authorizes a Lead-requested capture and subsequent removal.
// It is not the release commit: a failed normal capture leaves use available.
type ReleaseAction struct {
	SchemaVersion int    `json:"schema_version"`
	WorkspaceID   string `json:"workspace_id"`
	ID            string `json:"id"`
	Discard       bool   `json:"discard"`
	BudgetNanos   int64  `json:"budget_nanos"`
}

func (r *Resource) RecordReleaseAction(ctx context.Context, action ReleaseAction) (Subject, error) {
	if action.SchemaVersion != task.SchemaVersion || action.WorkspaceID != r.request.WorkspaceID || action.BudgetNanos <= 0 {
		return Subject{}, task.ErrIdentityMismatch
	}
	if err := task.ValidateTaskID(action.ID); err != nil {
		return Subject{}, err
	}
	data, err := task.MarshalCanonical(action)
	if err != nil {
		return Subject{}, err
	}
	_, err = r.control.PutContext(ctx, "release-"+action.ID+".json", action)
	return Subject{Kind: SubjectOperation, ID: action.ID, SHA256: task.ComputeSHA256(data)}, err
}

func (r *Resource) RecordReleaseCapture(ctx context.Context, reservation Reservation, subject Subject) error {
	if subject.Kind != SubjectOperation {
		return task.ErrIdentityMismatch
	}
	err := r.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var action ReleaseAction
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

func (r *Resource) ReleaseAction(ctx context.Context, subject Subject) (action ReleaseAction, resultErr error) {
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

func (r *Resource) ReadReleaseAction(ctx context.Context, id string) (action ReleaseAction, resultErr error) {
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

func (r *Resource) ReleaseCaptureAuthorized(ctx context.Context, obligation CaptureObligation) error {
	if obligation.Predecessor != nil || obligation.Subject.Kind != SubjectOperation {
		return task.ErrInvalidPermit
	}
	var action ReleaseAction
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
			return errors.Join(task.ErrIdentityMismatch, ErrIdentity)
		}
		return tx.Verify(name, action)
	})
}
