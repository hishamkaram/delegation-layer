package app

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func workspaceForgetPurpose(intent workspace.ForgetIntent) string {
	return "forget-" + intent.OperationID + ".json"
}

func workspaceForgetSubject(intent workspace.ForgetIntent) (workspace.Subject, error) {
	data, err := task.MarshalCanonical(intent)
	if err != nil {
		return workspace.Subject{}, err
	}
	return workspace.Subject{Kind: workspace.SubjectOperation, ID: intent.OperationID, SHA256: task.ComputeSHA256(data)}, nil
}

func workspaceRemovalSubject(release workspace.ReleaseRecord) (workspace.Subject, error) {
	data, err := task.MarshalCanonical(release)
	if err != nil {
		return workspace.Subject{}, err
	}
	return workspace.Subject{Kind: workspace.SubjectOperation, ID: release.OperationID, SHA256: task.ComputeSHA256(data)}, nil
}

// All creation, replay, submission, execution and result consumers use this
// binding. The intent owns the first deadline; explicit retries own their own
// immutable operation request and never replace that intent.
func validateWorkspaceActionRequest(ctx context.Context, resource *workspace.Resource, request workspace.OperationRequest) error {
	if request.Kind != workspace.OperationForget {
		return nil
	}
	intent, err := resource.ForgetIntent(ctx)
	if err != nil {
		return err
	}
	subject, err := workspaceForgetSubject(intent)
	if err != nil {
		return err
	}
	if request.WorkspaceID != resource.Request().WorkspaceID || request.Subject != subject {
		return task.ErrIdentityMismatch
	}
	first, err := resource.AssociatedOperation(ctx, workspaceForgetPurpose(intent))
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
	return validateWorkspaceActionRequest(ctx, resource, request)
}
