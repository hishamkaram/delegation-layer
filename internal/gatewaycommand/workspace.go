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
		if c.arguments.policySet {
			current, err := c.dependencies.Client.ListWorkspaces(ctx, c.profile)
			if err != nil {
				return response, err
			}
			var policies []deviceclient.ProviderPolicy
			for _, workspace := range current {
				if workspace.ID == c.arguments.WorkspaceID {
					policies = workspace.Providers
					break
				}
			}
			policies, err = patchProviderPolicies(c.arguments, policies, c.dependencies.Descriptions)
			if err != nil {
				return response, err
			}
			update.Providers = policies
		}
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
	if c.arguments.policySet {
		approval.Providers, err = patchProviderPolicies(c.arguments, local.Providers, c.dependencies.Descriptions)
		if err != nil {
			return response, err
		}
	}
	value, err := c.dependencies.Client.AllowWorkspace(ctx, c.profile, approval)
	response.Workspace = &value
	return response, err
}
