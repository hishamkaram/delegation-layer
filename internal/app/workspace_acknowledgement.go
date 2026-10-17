package app

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func workspaceCommandActionResponse(ctx context.Context, store *taskdir.Store, resource *workspace.Resource, action string, discard bool, ack workspacescore.ActionAcknowledgement, response Response, actionErr error) commandResult {
	if action == "status" {
		return workspaceCommandResponse(ctx, store, resource, ack.OperationID(), response, actionErr)
	}
	responseContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	var ackErr error
	if !ack.Authorized() {
		ack, ackErr = workspacescore.AcknowledgeAction(responseContext, store, resource, action, discard, ack.OperationID())
	}
	description, descriptionErr := workspacescore.Describe(responseContext, resource)
	// A prior use may have its own helper. It is not the requested action.
	description.OperationID, description.Operation = ack.OperationID(), nil
	if ack.OperationID() != "" {
		descriptionErr = errors.Join(descriptionErr, workspacescore.DescribeOperation(responseContext, store, resource, ack.OperationID(), &description))
	}
	response.Workspace = &description
	if ackErr != nil {
		return failed(response, errors.Join(actionErr, descriptionErr, ackErr), 1)
	}
	return finishWorkspaceActionResponse(responseContext, response, action, ack, actionErr, descriptionErr)
}

func finishWorkspaceActionResponse(ctx context.Context, response Response, action string, ack workspacescore.ActionAcknowledgement, actionErr, descriptionErr error) commandResult {
	result := finishWorkspaceCommandResponse(ctx, response, actionErr, descriptionErr)
	if result.code == 3 {
		if ack.Authorized() {
			result.response.Workspace.NextAction = "watch_workspace"
			result.response.Workspace.OperationID = ack.OperationID()
		} else {
			result.response.Workspace.ReasonCode = "requested_action_not_authorized"
			result.response.Workspace.NextAction = "retry_" + action
			result.response.Workspace.OperationID, result.response.Workspace.Operation = "", nil
		}
	}
	return result
}
