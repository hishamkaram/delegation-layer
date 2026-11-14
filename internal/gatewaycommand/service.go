package gatewaycommand

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
)

// Service manages only this profile's connector registration. Its stop result
// does not describe the lifetime of Delegate-owned tasks.
type Service interface {
	Install(context.Context) (gatewayservice.Status, error)
	Start(context.Context) (gatewayservice.Status, error)
	Stop(context.Context) (gatewayservice.Status, error)
	Status(context.Context) (gatewayservice.Status, error)
	Uninstall(context.Context) (gatewayservice.Status, error)
}

// ServiceFactory composes the selected OS manager at the command boundary.
type ServiceFactory interface {
	Open(profile, executable string) (Service, error)
}

func (c invocation) serviceManager(executable string) (Service, error) {
	if c.dependencies.Services != nil {
		return c.dependencies.Services.Open(c.profile, executable)
	}
	return c.nativeServiceManager(executable)
}

func (c invocation) service(ctx context.Context, response Response) (Response, error) {
	local, err := c.dependencies.Client.LocalContext(ctx, c.profile)
	if err != nil {
		return response, err
	}
	if c.arguments.ServiceAction == "install" || c.arguments.ServiceAction == "start" {
		if _, err = c.selectedContext(ctx); err != nil {
			return response, err
		}
	}
	manager, err := c.serviceManager(local.DelegateExecutable)
	if err != nil {
		return response, err
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var status gatewayservice.Status
	switch c.arguments.ServiceAction {
	case "install":
		status, err = manager.Install(bounded)
	case "start":
		status, err = manager.Start(bounded)
	case "stop":
		status, err = manager.Stop(bounded)
	case "status":
		status, err = manager.Status(bounded)
	case "uninstall":
		status, err = manager.Uninstall(bounded)
	default:
		err = errors.New("unsupported_gateway_service_action")
	}
	response.Service = &status
	return response, err
}

func (c invocation) installAndStart(ctx context.Context, response Response) (Response, error) {
	local, err := c.selectedContext(ctx)
	if err != nil {
		return response, err
	}
	manager, err := c.serviceManager(local.DelegateExecutable)
	if err != nil {
		return response, err
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	status, err := manager.Install(bounded)
	response.Service = &status
	if err != nil {
		return response, err
	}
	status, err = manager.Start(bounded)
	response.Service = &status
	return response, err
}

func (c invocation) serve(ctx context.Context, response Response) (Response, error) {
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := c.selectedContext(startup); err != nil {
		return response, err
	}
	var runtime Runtime
	err := gatewayservice.WithProfileLock(startup, c.profile, func() error {
		var openErr error
		runtime, openErr = c.dependencies.Client.OpenRuntime(startup, c.profile)
		return openErr
	})
	if err != nil {
		if runtime != nil {
			err = errors.Join(err, runtime.Close())
		}
		return response, err
	}
	cancel()
	serveErr := runtime.Serve(ctx)
	closeErr := runtime.Close()
	if deviceclient.IsExpectedShutdown(serveErr, closeErr) {
		return response, nil
	}
	return response, errors.Join(serveErr, closeErr)
}
