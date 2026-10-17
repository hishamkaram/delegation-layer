package workspaces

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func BeginVerification(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, deps OperationOptions, arguments []string, budget time.Duration) (ack ActionAcknowledgement, resultErr error) {
	if err := ReconcileUse(ctx, store, resource, deps); err != nil {
		return ack, err
	}
	id, err := randomid.NewTaskID()
	if err != nil {
		return ack, err
	}
	data, err := task.MarshalCanonical(struct {
		WorkspaceID string   `json:"workspace_id"`
		ID          string   `json:"id"`
		Arguments   []string `json:"arguments"`
	}{resource.Request().WorkspaceID, id, arguments})
	if err != nil {
		return ack, err
	}
	authorize := func(scheduling context.Context, operation *workspace.Operation) error {
		ack.operationID = operation.Request().OperationID
		subject := workspace.Subject{Kind: workspace.SubjectOperation, ID: ack.operationID, SHA256: operation.Digest()}
		reservation, reserveErr := resource.Reserve(scheduling, subject)
		if reserveErr != nil {
			return reserveErr
		}
		ack.authorized = true
		return RecordVerificationUse(scheduling, resource, reservation, operation)
	}
	operation, err := ScheduleOperation(ctx, store, resource, deps, OperationSpec{Purpose: "verify-" + id + ".json", Kind: workspace.OperationVerify, Subject: workspace.Subject{Kind: workspace.SubjectAdmission, ID: id, SHA256: task.ComputeSHA256(data)}, Budget: budget, Arguments: arguments}, authorize)
	defer func() { resultErr = errors.Join(resultErr, CloseOperation(operation)) }()
	if err != nil {
		return ack, err
	}
	return ack, workspace.ErrPending
}
