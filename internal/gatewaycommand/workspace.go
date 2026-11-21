package gatewaycommand

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (c invocation) workspace(ctx context.Context, response Response) (Response, error) {
	switch c.arguments.WorkspaceAction {
	case "list":
		values, err := c.dependencies.Client.ListWorkspaces(ctx, c.profile)
		response.Workspaces = &values
		return response, err
	case "allow":
		return c.allowWorkspace(ctx, response)
	case "update":
		update := deviceclient.WorkspaceUpdate{Alias: c.arguments.Alias}
		value, err := c.dependencies.Client.UpdateWorkspace(ctx, c.profile, c.arguments.WorkspaceID, c.arguments.ExpectedRevision, update)
		response.Workspace = &value
		return response, err
	case "remove":
		value, err := c.dependencies.Client.RemoveWorkspace(ctx, c.profile, c.arguments.WorkspaceID, c.arguments.ExpectedRevision)
		response.Workspace = &value
		return response, err
	default:
		return response, errors.New("unsupported_gateway_workspace_action")
	}
}

func (c invocation) allowWorkspace(ctx context.Context, response Response) (Response, error) {
	local, err := c.selectedContext(ctx)
	if err != nil {
		return response, err
	}
	directory, token, err := inspectWorkspace(ctx, local.DelegateRoot, local.DelegateRootContext, c.arguments.Directory, c.dependencies.Predicates)
	if err != nil {
		return response, err
	}
	alias := c.arguments.Alias
	if alias == "" {
		alias = filepath.Base(directory)
	}
	if err = task.ValidateConnectorID(alias); err != nil {
		return response, errors.New("workspace requires a valid explicit --alias")
	}
	approval := deviceclient.WorkspaceApproval{Alias: alias, Directory: directory, NativeContext: token}
	value, err := c.dependencies.Client.AllowWorkspace(ctx, c.profile, approval)
	response.Workspace = &value
	return response, err
}
