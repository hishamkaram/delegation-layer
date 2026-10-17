package workspaces

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// All creation, replay, submission, execution and result consumers use this
// binding. The intent owns the first deadline; explicit retries own their own
// immutable operation request and never replace that intent.
func ValidateActionRequest(ctx context.Context, resource *workspace.Resource, request workspace.OperationRequest) error {
	if request.Kind != workspace.OperationForget {
		return nil
	}
	intent, err := resource.ForgetIntent(ctx)
	if err != nil {
		return err
	}
	subject, err := ForgetSubject(intent)
	if err != nil {
		return err
	}
	if request.WorkspaceID != resource.Request().WorkspaceID || request.Subject != subject {
		return task.ErrIdentityMismatch
	}
	first, err := resource.AssociatedOperation(ctx, ForgetPurpose(intent))
	if err != nil {
		return err
	}
	if first.Kind != workspace.OperationForget || (first.OperationID == request.OperationID && request.Deadline != intent.Deadline) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateStoredWorkspaceActionRequest(ctx context.Context, store *taskdir.Store, request workspace.OperationRequest) (resultErr error) {
	if request.Kind != workspace.OperationForget {
		return nil
	}
	resource, err := workspace.LoadResource(ctx, store, request.WorkspaceID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, resource.Close()) }()
	return ValidateActionRequest(ctx, resource, request)
}
