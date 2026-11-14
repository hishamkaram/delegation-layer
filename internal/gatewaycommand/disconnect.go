package gatewaycommand

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

func (c invocation) disconnect(ctx context.Context, response Response) (Response, error) {
	local, err := c.dependencies.Client.LocalContext(ctx, c.profile)
	if err != nil {
		return response, err
	}
	manager, err := c.serviceManager(local.DelegateExecutable)
	if err != nil {
		return response, err
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	status, err := manager.Uninstall(bounded)
	response.Service = &status
	if err != nil || !c.arguments.Revoke {
		return response, err
	}
	inactive, err := c.dependencies.Client.RuntimeInactive(ctx, c.profile)
	if err != nil || !inactive {
		return response, errors.Join(gatewayservice.ErrPending, err)
	}
	auth, finish := context.WithTimeout(ctx, 10*time.Minute)
	defer finish()
	owner, err := c.currentExecutable()
	if err != nil {
		return response, err
	}
	err = c.dependencies.Client.RevokeDevice(auth, c.profile, c.challenge(owner))
	response.Revoked = err == nil
	return response, err
}

func (c invocation) nativeServiceManager(executable string) (*gatewayservice.Manager, error) {
	var directory, managerExecutable string
	var platform gatewayservice.Platform
	switch runtime.GOOS {
	case "linux":
		platform = gatewayservice.Linux
		configDirectory, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		directory = filepath.Join(configDirectory, "systemd", "user")
		managerExecutable, err = verifiedexec.LookPath("systemctl", c.dependencies.Environment)
		if err != nil {
			return nil, errors.Join(gatewayservice.ErrManager, err)
		}
	case "darwin":
		platform = gatewayservice.MacOS
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		directory = filepath.Join(home, "Library", "LaunchAgents")
		managerExecutable = "/bin/launchctl"
	default:
		return nil, errors.New("gateway_service_platform_unsupported")
	}
	directory, err := config.CanonicalizePath(directory)
	if err != nil {
		return nil, err
	}
	managerExecutable, err = verifiedexec.LocatePath(managerExecutable)
	if err != nil {
		return nil, errors.Join(gatewayservice.ErrManager, err)
	}
	owner, err := c.currentExecutable()
	if err != nil {
		return nil, err
	}
	runner, err := gatewayservice.NewNativeRunner(owner, c.dependencies.Environment)
	if err != nil {
		return nil, err
	}
	return gatewayservice.New(gatewayservice.Config{Platform: platform, ProfileDirectory: c.profile, Executable: executable, Directory: directory, ManagerExecutable: managerExecutable, UID: os.Getuid(), Path: verifiedexec.EnvironmentValue(c.dependencies.Environment, "PATH"), Runner: runner, RuntimeInactive: func(ctx context.Context) (bool, error) { return c.dependencies.Client.RuntimeInactive(ctx, c.profile) }})
}
