package gatewaycommand

import (
	"context"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
)

// Client is the command consumer's SDK boundary. Native root/workspace
// inspection is deliberately outside this network/profile owner.
type Client interface {
	Connect(context.Context, deviceclient.ConnectOptions) (deviceclient.ConnectionStatus, error)
	Status(context.Context, string) (deviceclient.ConnectionStatus, error)
	LocalContext(context.Context, string) (deviceclient.LocalContextInfo, error)
	ListWorkspaces(context.Context, string) ([]deviceclient.Workspace, error)
	AllowWorkspace(context.Context, string, deviceclient.WorkspaceApproval) (deviceclient.Workspace, error)
	UpdateWorkspace(context.Context, string, string, uint64, deviceclient.WorkspaceUpdate) (deviceclient.Workspace, error)
	RemoveWorkspace(context.Context, string, string, uint64) (deviceclient.Workspace, error)
	RuntimeInactive(context.Context, string) (bool, error)
	OpenRuntime(context.Context, string) (Runtime, error)
	RevokeDevice(context.Context, string, func(context.Context, deviceclient.Challenge) error) error
}

// Runtime owns connector connectivity and joins its CLI requests on Close.
// It never owns provider task lifetime or accepted outcomes.
type Runtime interface {
	Serve(context.Context) error
	Close() error
}

type sdkClient struct{}

func (sdkClient) Connect(ctx context.Context, o deviceclient.ConnectOptions) (deviceclient.ConnectionStatus, error) {
	return deviceclient.Connect(ctx, o)
}

func (sdkClient) Status(ctx context.Context, p string) (deviceclient.ConnectionStatus, error) {
	return deviceclient.Status(ctx, p)
}

func (sdkClient) LocalContext(ctx context.Context, p string) (deviceclient.LocalContextInfo, error) {
	return deviceclient.LocalContext(ctx, p)
}

func (sdkClient) ListWorkspaces(ctx context.Context, p string) ([]deviceclient.Workspace, error) {
	return deviceclient.ListWorkspaces(ctx, p)
}

func (sdkClient) AllowWorkspace(ctx context.Context, p string, a deviceclient.WorkspaceApproval) (deviceclient.Workspace, error) {
	return deviceclient.AllowWorkspace(ctx, p, a)
}

func (sdkClient) UpdateWorkspace(ctx context.Context, p, id string, r uint64, u deviceclient.WorkspaceUpdate) (deviceclient.Workspace, error) {
	return deviceclient.UpdateWorkspace(ctx, p, id, r, u)
}

func (sdkClient) RemoveWorkspace(ctx context.Context, p, id string, r uint64) (deviceclient.Workspace, error) {
	return deviceclient.RemoveWorkspace(ctx, p, id, r)
}

func (sdkClient) RuntimeInactive(ctx context.Context, p string) (bool, error) {
	return deviceclient.RuntimeInactive(ctx, p)
}

func (sdkClient) OpenRuntime(ctx context.Context, p string) (Runtime, error) {
	return deviceclient.OpenRuntime(ctx, p)
}

func (sdkClient) RevokeDevice(ctx context.Context, p string, present func(context.Context, deviceclient.Challenge) error) error {
	return deviceclient.RevokeDevice(ctx, p, present)
}
