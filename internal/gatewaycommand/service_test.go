package gatewaycommand

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
)

type commandServices struct{ service Service }

func (s commandServices) Open(string, string) (Service, error) { return s.service, nil }

type commandService struct {
	calls      []string
	installErr error
	startErr   error
	stopErr    error
}

func (s *commandService) observe(ctx context.Context, action string, err error) (gatewayservice.Status, error) {
	if _, finite := ctx.Deadline(); !finite {
		return gatewayservice.Status{}, errors.New("service request has no deadline")
	}
	s.calls = append(s.calls, action)
	status := gatewayservice.Status{Installed: true, State: gatewayservice.StateRegistered}
	if action == "stop" || action == "uninstall" {
		status.State = gatewayservice.StateInactive
	}
	return status, err
}

func (s *commandService) Install(ctx context.Context) (gatewayservice.Status, error) {
	return s.observe(ctx, "install", s.installErr)
}

func (s *commandService) Start(ctx context.Context) (gatewayservice.Status, error) {
	return s.observe(ctx, "start", s.startErr)
}

func (s *commandService) Stop(ctx context.Context) (gatewayservice.Status, error) {
	return s.observe(ctx, "stop", s.stopErr)
}

func (s *commandService) Status(ctx context.Context) (gatewayservice.Status, error) {
	return s.observe(ctx, "status", nil)
}

func (s *commandService) Uninstall(ctx context.Context) (gatewayservice.Status, error) {
	return s.observe(ctx, "uninstall", s.stopErr)
}

func TestGatewayConnectInstallsStartsAndRetainsEnrollmentOnManagerFailure(t *testing.T) {
	for _, failInstall := range []bool{false, true} {
		t.Run(map[bool]string{false: "registered", true: "manager-unavailable"}[failInstall], func(t *testing.T) {
			client := &commandClient{}
			service := &commandService{}
			if failInstall {
				service.installErr = gatewayservice.ErrManager
			}
			deps := commandDependencies(t, client)
			deps.Services = commandServices{service: service}
			a := Arguments{Action: "connect", GatewayURL: "https://gateway.example", Headless: true, Permission: "read-only", MaximumBudget: 60 * time.Second, DeviceName: "test-device"}
			var output, diagnostic bytes.Buffer
			code := Run(boundedTestContext(t), a, filepath.Join(t.TempDir(), "state"), true, &output, &diagnostic, deps)
			wantCalls, wantCode := []string{"install", "start"}, 0
			if failInstall {
				wantCalls, wantCode = []string{"install"}, 1
			}
			if code != wantCode || client.connectCalls != 1 || !reflect.DeepEqual(service.calls, wantCalls) {
				t.Fatalf("unexpected connector startup: exit%d enrolled%d manager%v", code, client.connectCalls, service.calls)
			}
			if !bytes.Contains(output.Bytes(), []byte(`"credential_state":"valid"`)) || !bytes.Contains(output.Bytes(), []byte(`"online":"unknown"`)) {
				t.Fatalf("enrollment lost or manager registration treated as connectivity: %s", output.Bytes())
			}
			if failInstall && !bytes.Contains(output.Bytes(), []byte(`"failure":"gateway_service_manager_failed"`)) {
				t.Fatalf("manager prerequisite failure lost its actionable code: %s", output.Bytes())
			}
		})
	}
}

type commandRuntime struct {
	profile  string
	serveErr error
	closeErr error
	closed   bool
	served   bool
}

func (r *commandRuntime) Serve(ctx context.Context) error {
	r.served = true
	if _, deadline := ctx.Deadline(); deadline {
		return errors.New("foreground runtime inherited login deadline")
	}
	probe, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := gatewayservice.WithProfileLock(probe, r.profile, func() error { return nil }); err != nil {
		return err
	}
	return r.serveErr
}

func (r *commandRuntime) Close() error { r.closed = true; return r.closeErr }

type runtimeClient struct {
	commandClient
	runtime *commandRuntime
}

func (c *runtimeClient) OpenRuntime(context.Context, string) (Runtime, error) { return c.runtime, nil }

func (c *runtimeClient) Connect(ctx context.Context, options deviceclient.ConnectOptions) (deviceclient.ConnectionStatus, error) {
	status, err := c.commandClient.Connect(ctx, options)
	if err == nil {
		err = os.Mkdir(options.ProfileDirectory, 0o700)
		c.runtime.profile = options.ProfileDirectory
	}
	return status, err
}

func TestGatewayConnectForegroundDoesNotInstallService(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runtime := &commandRuntime{}
	client := &runtimeClient{runtime: runtime}
	service := &commandService{}
	deps := commandDependencies(t, client)
	deps.Services = commandServices{service: service}
	a := Arguments{Action: "connect", GatewayURL: "https://gateway.example", Foreground: true, Headless: true, Permission: "read-only", MaximumBudget: 60 * time.Second, DeviceName: "test-device"}
	var output, diagnostic bytes.Buffer
	code := Run(ctx, a, filepath.Join(t.TempDir(), "state"), true, &output, &diagnostic, deps)
	if code != 0 || client.connectCalls != 1 || !runtime.served || !runtime.closed || len(service.calls) != 0 {
		t.Fatalf("foreground ownership failed: exit%d enrolled%d served%t closed%t manager%v output%s", code, client.connectCalls, runtime.served, runtime.closed, service.calls, output.Bytes())
	}
}

func TestGatewayForegroundReleasesStartupLockAndJoinsRuntime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	root, observed, err := rootContext(boundedTestContext(t), filepath.Join(t.TempDir(), "state"), predicate.Default(), true)
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "gateway")
	if err = os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	serveFault, closeFault := errors.New("serve fault"), errors.New("close fault")
	runtime := &commandRuntime{profile: profile, serveErr: serveFault, closeErr: closeFault}
	client := &runtimeClient{runtime: runtime}
	deps := commandDependencies(t, client)
	client.local.DelegateRoot, client.local.DelegateRootContext = root, observed.RootToken
	call := invocation{profile: profile, dependencies: deps}
	_, err = call.serve(ctx, Response{})
	if !runtime.served || !runtime.closed || !errors.Is(err, serveFault) || !errors.Is(err, closeFault) {
		t.Fatalf("runtime not joined outside startup lock: served%t closed%t err%v", runtime.served, runtime.closed, err)
	}
	runtime.serveErr, runtime.closeErr = context.Canceled, nil
	runtime.served, runtime.closed = false, false
	_, err = call.serve(ctx, Response{})
	if !runtime.served || !runtime.closed || !errors.Is(err, context.Canceled) {
		t.Fatalf("unqualified cancellation became successful shutdown: served%t closed%t err%v", runtime.served, runtime.closed, err)
	}
}

func TestGatewayCleanupUsesCurrentHelperAndKeepsMissingSavedSource(t *testing.T) {
	root, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	profile := filepath.Join(root, "profile")
	if err = os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	// Metadata-only fixture. Correct source validation rejects before execution.
	if err = os.WriteFile(filepath.Join(root, "systemctl"), []byte("manager fixture; never executed\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	deps := commandDependencies(t, &commandClient{})
	deps.Environment = []string{"PATH=" + root}
	call := invocation{profile: profile, dependencies: deps}
	manager, err := call.nativeServiceManager(filepath.Join(root, "removed-delegate"))
	if err != nil {
		t.Fatalf("missing saved executable blocked cleanup manager: %v", err)
	}
	if _, err = manager.Install(boundedTestContext(t)); !errors.Is(err, gatewayservice.ErrConfiguration) {
		t.Fatalf("cleanup helper silently replaced saved service source: %v", err)
	}
}
