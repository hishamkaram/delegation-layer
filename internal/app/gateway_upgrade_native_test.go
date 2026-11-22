package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/gatewaycommand"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Native store, CLI preflight, resident, daemon and operation control are real.
// SDK and OS connector/GUI boundaries are doubled. Each App invocation captures
// its own authority; the GUI route does not reuse the parent's capture.
func TestGatewayUpgradeNativeMaintenanceComposition(t *testing.T) {
	t.Run("gui_routed", func(t *testing.T) { testGatewayUpgradeNativeComposition(t, true) })
	if runtime.GOOS != "darwin" {
		t.Run("in_process", func(t *testing.T) { testGatewayUpgradeNativeComposition(t, false) })
	}
}

func testGatewayUpgradeNativeComposition(t *testing.T, guiRouted bool) {
	t.Helper()
	client, daemon := requireRuntimeRestartCLINativeContext(t)
	f := newRuntimeRestartCLINative(t, client, daemon)
	prepareGatewayUpgradeNativeKeg(t, f)
	f.build(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	f.ensureOwner(t, ctx)
	t.Cleanup(func() { closeQuietColdUpgradeFixture(t, f) })
	originalOwner, originalDaemon := f.preflight(t, ctx), f.executionTarget(t, ctx)
	profile := newGatewayUpgradeNativeProfile(t, ctx, f)
	rootBytes := readGatewayUpgradeNativeRoot(t, f)

	prepared := coldUpgradeCLIStep(t, ctx, f, "", infrastructure.UpgradeAwaitingInstallation, "ensure-owner", "--prepare-upgrade")
	operation := prepared.OperationID
	t.Cleanup(func() { closeGatewayUpgradeNativeOperation(ctx, t, f, operation) })
	if prepared.Mode != string(infrastructure.UpgradeOperationRequired) {
		t.Fatal("public preparation did not retain the expected proactive operation")
	}

	services := &gatewayUpgradeNativeServices{profile: profile.directory}
	response := runGatewayUpgradeNativeComposition(t, ctx, f, profile, services, operation, guiRouted)
	assertGatewayUpgradeNativeCompletion(t, ctx, f, profile, services, response, operation)
	if !bytes.Equal(rootBytes, readGatewayUpgradeNativeRoot(t, f)) {
		t.Fatal("Gateway maintenance rewrote the original Native root record")
	}
	if current := f.preflight(t, ctx); current != originalOwner {
		t.Fatal("same-installation Gateway maintenance unnecessarily replaced the serving resident")
	}
	same, err := lifetime.SameProcess(originalDaemon, f.executionTarget(t, ctx))
	if err != nil || same {
		t.Fatalf("authorized installation handoff did not start a fresh daemon: same=%t err=%v", same, err)
	}
	assertGatewayUpgradeCompletedReplayPreservesCurrentOperation(t, ctx, f, profile, operation)
	t.Log("real App Gateway composition completed the same public Native operation; no provider task launched")
}

func assertGatewayUpgradeCompletedReplayPreservesCurrentOperation(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, profile *gatewayUpgradeNativeProfile, completed string) {
	t.Helper()
	daemon := f.executionTarget(t, ctx)
	prepared := coldUpgradeCLIStep(t, ctx, f, "", infrastructure.UpgradeAwaitingInstallation, "ensure-owner", "--prepare-upgrade")
	t.Cleanup(func() { closeGatewayUpgradeNativeOperation(ctx, t, f, prepared.OperationID) })
	if prepared.OperationID == completed {
		t.Fatal("new public preparation reused the completed operation")
	}
	before := observeGatewayUpgradeNativeReplay(t, ctx, f, prepared.OperationID, daemon)
	maintenance := gatewayNativeMaintenance{dependencies: Dependencies{
		Environment: f.environment, RunnerExecutable: filepath.Join(f.bin, "delegate-run"), InitialSupervisorExecutable: f.originalClient,
	}.withCapturedEnvironmentContext(ctx).normalized()}
	observed, err := maintenance.Ensure(ctx, gatewaycommand.NativeMaintenanceRequest{Root: f.root, ExpectedRoot: profile.source.DelegateRootContext, OperationID: completed})
	if err != nil || observed.OperationID != completed || observed.Pending {
		t.Fatalf("completed exact operation replay failed while another operation is current: observed=%+v err=%v", observed, err)
	}
	requireGatewayUpgradeReplayCallerRefusal(t, ctx, f, profile.source.DelegateRootContext, completed)
	after := observeGatewayUpgradeNativeReplay(t, ctx, f, prepared.OperationID, daemon)
	if before.owner != after.owner || before.operation != after.operation || before.binding != after.binding || !os.SameFile(before.endpoint, after.endpoint) {
		t.Fatal("completed operation replay changed the current operation, resident or endpoint")
	}
}

func requireGatewayUpgradeReplayCallerRefusal(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, token, completed string) {
	t.Helper()
	for _, kind := range []string{"contained completed replay", "origin completed replay"} {
		t.Run(kind, func(t *testing.T) {
			want := error(lifetime.ErrTaskContained)
			if kind == "contained completed replay" {
				t.Setenv("DELEGATION_LAYER_PROCESS_SCOPE", "pg:"+strings.Repeat("d", 32))
			} else {
				origin, err := infrastructure.NewOrigin(f.root, f.rootID)
				if err != nil {
					t.Fatal(err)
				}
				environment, err := infrastructure.WithOrigin(nil, origin)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range environment {
					key, value, _ := strings.Cut(entry, "=")
					t.Setenv(key, value)
				}
				want = infrastructure.ErrOwnerRequest
			}
			maintenance := gatewayNativeMaintenance{dependencies: Dependencies{Environment: f.environment}.withCapturedEnvironmentContext(ctx).normalized()}
			_, err := maintenance.Ensure(ctx, gatewaycommand.NativeMaintenanceRequest{Root: f.root, ExpectedRoot: token, OperationID: completed})
			if !errors.Is(err, want) {
				t.Fatalf("completed replay bypassed original caller refusal: %v", err)
			}
		})
	}
}

type gatewayUpgradeNativeReplay struct {
	owner     infrastructure.StartupReference
	operation pueue.PrivateUpgradeObservation
	binding   task.SupervisorRef
	endpoint  os.FileInfo
}

func observeGatewayUpgradeNativeReplay(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, operation string, daemon lifetime.ProcessRef) gatewayUpgradeNativeReplay {
	t.Helper()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, f.root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	owner, err := infrastructure.ProbeOwner(ctx, reader)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := reader.PinRootContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	observed, found, err := pueue.ObservePrivateUpgrade(ctx, pin, pueue.UpgradeOperationID(operation))
	if err != nil || !found || observed.Completed || string(observed.OperationID) != operation {
		t.Fatalf("current Native operation unavailable: found=%t err=%v", found, err)
	}
	endpoint, err := os.Lstat(filepath.Join(f.root, ".supervisor", "run", "pueue.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if present, observeErr := lifetime.ProcessPresent(daemon); observeErr != nil || !present {
		t.Fatalf("prepared current operation lost its exact existing daemon: present=%t err=%v", present, observeErr)
	}
	return gatewayUpgradeNativeReplay{owner: owner, operation: observed, binding: f.binding(t), endpoint: endpoint}
}

func prepareGatewayUpgradeNativeKeg(t *testing.T, f *runtimeRestartCLINative) {
	t.Helper()
	f.bin = filepath.Join(f.base, "new-keg", "bin")
	if err := os.MkdirAll(f.bin, 0o700); err != nil {
		t.Fatal(err)
	}
	f.environment = runtimeRestartCLIEnvironment(f.environment, map[string]string{
		"PATH": f.bin + string(os.PathListSeparator) + filepath.Dir(f.originalClient) + string(os.PathListSeparator) + "/usr/bin:/bin",
	})
}

func newGatewayUpgradeNativeProfile(t *testing.T, ctx context.Context, f *runtimeRestartCLINative) *gatewayUpgradeNativeProfile {
	t.Helper()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, f.root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	observed, inspectErr := reader.InspectConnectorContext(ctx, "")
	if err = errors.Join(inspectErr, reader.Close()); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(f.root, "gateway")
	if err = os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	oldExecutable := filepath.Join(f.base, "old-keg", "delegate")
	if err = os.Mkdir(filepath.Dir(oldExecutable), 0o700); err != nil {
		t.Fatal(err)
	}
	// This old profile source is metadata only and is never executed. The
	// selected Native CLI and runner were built in place in the new keg.
	if err = os.WriteFile(oldExecutable, []byte("#!/bin/sh\nexit 64\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(oldExecutable); err != nil {
		t.Fatal(err)
	}
	return &gatewayUpgradeNativeProfile{
		directory: directory,
		source: deviceclient.LocalContextInfo{
			DelegateExecutable: oldExecutable, DelegateRoot: f.root, DelegateRootContext: observed.RootToken,
		},
		selected: filepath.Join(f.bin, "delegate"),
	}
}

func readGatewayUpgradeNativeRoot(t *testing.T, f *runtimeRestartCLINative) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func runGatewayUpgradeNativeComposition(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, profile *gatewayUpgradeNativeProfile, services *gatewayUpgradeNativeServices, operation string, guiRouted bool) gatewaycommand.Response {
	t.Helper()
	dependencies := Dependencies{
		Gateway: &gatewaycommand.Dependencies{
			Client: profile, Executable: profile.selected, Services: services, Environment: f.environment,
		},
		Environment: f.environment, RunnerExecutable: filepath.Join(f.bin, "delegate-run"), InitialSupervisorExecutable: f.originalClient,
	}
	var gui *gatewayGUIFixture
	if guiRouted {
		gui = newGatewayGUIFixture(t, ctx, f.base, dependencies, profile)
		dependencies.Gateway.GUIUpgrade = gui.manager
		gui.dependencies = dependencies
	}
	arguments := []string{"gateway", "upgrade", "--root", f.root, "--profile", profile.directory, "--accept-execution-policy-removal", "--json"}
	for range 4 {
		var output, diagnostic bytes.Buffer
		code := RunContext(ctx, arguments, &output, &diagnostic, dependencies)
		if gui != nil {
			gui.observeFinishedInvocation()
		}
		var response gatewaycommand.Response
		if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Upgrade == nil {
			t.Fatalf("Gateway composition response unavailable: exit=%d err=%v", code, err)
		}
		if bytes.Contains(output.Bytes(), []byte(profile.source.DelegateRootContext)) {
			t.Fatal("Gateway response exposed the opaque Native root token")
		}
		if code == 0 {
			if gui != nil {
				gui.assertCompleted(t, ctx, arguments, dependencies, response, operation)
			}
			return response
		}
		if !gatewayUpgradeNativePending(response, code, operation) {
			t.Fatalf("actual App Gateway upgrade failed: exit=%d phase=%s failure=%s next=%s observed_operation=%s expected_operation=%s", code, response.Upgrade.Phase, response.Failure, response.NextAction, response.Upgrade.Supervisor.OperationID, operation)
		}
		if profile.migrated || profile.updated {
			t.Fatal("pending Native operation committed profile migration or source handoff")
		}
	}
	t.Fatal("same Gateway upgrade remained pending within its finite public workflow")
	return gatewaycommand.Response{}
}

func gatewayUpgradeNativePending(response gatewaycommand.Response, code int, operation string) bool {
	return code == 3 && response.Failure == "gateway_upgrade_pending" && response.NextAction == "retry_gateway_upgrade_same_profile" &&
		response.Upgrade.Phase == "supervisor_transition" && response.Upgrade.Supervisor.Pending && response.Upgrade.Supervisor.OperationID == operation
}

func assertGatewayUpgradeNativeCompletion(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, profile *gatewayUpgradeNativeProfile, services *gatewayUpgradeNativeServices, response gatewaycommand.Response, operation string) {
	t.Helper()
	u := response.Upgrade
	if u.Phase != "complete" || !u.ServiceRestarted || !u.DeviceIdentityPreserved || u.Supervisor.Pending || u.Supervisor.OperationID != operation {
		t.Fatalf("Gateway did not complete the exact Native operation: phase=%s operation=%s pending=%t", u.Phase, u.Supervisor.OperationID, u.Supervisor.Pending)
	}
	if response.Online != "unknown" || response.ExecutionReadiness != "unknown" || !profile.migrated || !profile.updated {
		t.Fatal("Gateway lost committed source state or invented live execution readiness")
	}
	requireGatewayUpgradeNativeJournal(t, ctx, f, operation)
	services.assertHandoff(t, profile.originalExecutable, profile.selected)
}

func requireGatewayUpgradeNativeJournal(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, operation string) {
	t.Helper()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, f.root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	pin, pinErr := reader.PinRootContext(ctx)
	if pinErr != nil {
		t.Fatal(errors.Join(pinErr, reader.Close()))
	}
	observed, found, observeErr := pueue.ObservePrivateUpgrade(ctx, pin, pueue.UpgradeOperationID(operation))
	if err = errors.Join(observeErr, reader.Close()); err != nil || !found || !observed.Completed || string(observed.OperationID) != operation {
		t.Fatalf("canonical Native journal did not retain exact completion: found=%t err=%v", found, err)
	}
}

func closeGatewayUpgradeNativeOperation(parent context.Context, t *testing.T, f *runtimeRestartCLINative, operation string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
	defer cancel()
	completed := coldUpgradeCLIStep(t, ctx, f, operation, infrastructure.UpgradeMigrating, "ensure-owner", "--complete-upgrade", operation)
	if !completed.Completed {
		t.Error("cleanup retained an uncompleted exact Native operation")
		return
	}
	// Remember any replacement before the existing fixture joins every owner.
	f.preflight(t, ctx)
}

type gatewayUpgradeNativeProfile struct {
	directory, selected, originalExecutable string
	source                                  deviceclient.LocalContextInfo
	migrated, updated                       bool
}

func (p *gatewayUpgradeNativeProfile) check(ctx context.Context, directory string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if directory != p.directory {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (p *gatewayUpgradeNativeProfile) ProfileMigrationSource(ctx context.Context, directory string) (deviceclient.LocalContextInfo, error) {
	if p.originalExecutable == "" {
		p.originalExecutable = p.source.DelegateExecutable
	}
	return p.source, p.check(ctx, directory)
}

func (p *gatewayUpgradeNativeProfile) LocalContext(ctx context.Context, directory string) (deviceclient.LocalContextInfo, error) {
	return p.source, p.check(ctx, directory)
}

func (p *gatewayUpgradeNativeProfile) PreviewProfileMigration(ctx context.Context, directory string) (deviceclient.ProfileMigrationPreview, error) {
	return deviceclient.ProfileMigrationPreview{FromVersion: 2, ToVersion: 3, GatewayID: "native-upgrade-fixture"}, p.check(ctx, directory)
}

func (p *gatewayUpgradeNativeProfile) RuntimeInactive(ctx context.Context, directory string) (bool, error) {
	return true, p.check(ctx, directory)
}

func (p *gatewayUpgradeNativeProfile) Status(ctx context.Context, directory string) (deviceclient.ConnectionStatus, error) {
	return deviceclient.ConnectionStatus{GatewayID: "native-upgrade-fixture", GatewayURL: "https://gateway.invalid", DeviceID: "native-upgrade-device", OwnerPrincipal: "native-upgrade-owner", CredentialGeneration: 1, CredentialState: "active"}, p.check(ctx, directory)
}

func (p *gatewayUpgradeNativeProfile) MigrateProfile(ctx context.Context, options deviceclient.ProfileMigrationOptions) (deviceclient.ProfileMigrationPreview, error) {
	if err := p.check(ctx, options.ProfileDirectory); err != nil {
		return deviceclient.ProfileMigrationPreview{}, err
	}
	if !options.AcceptExecutionPolicyRemoval {
		return deviceclient.ProfileMigrationPreview{}, deviceclient.ErrMigrationAcceptanceRequired
	}
	p.migrated = true
	return deviceclient.ProfileMigrationPreview{FromVersion: 2, ToVersion: 3, GatewayID: "native-upgrade-fixture"}, nil
}

func (p *gatewayUpgradeNativeProfile) UpdateInstallationSelection(ctx context.Context, directory string, expected, selected deviceclient.InstallationSelection) error {
	if err := p.check(ctx, directory); err != nil {
		return err
	}
	if !p.migrated || expected != p.source.InstallationSelection() || selected.DelegateExecutable != p.selected || selected.DelegateRoot != expected.DelegateRoot || selected.DelegateRootContext != expected.DelegateRootContext {
		return task.ErrRequestConflict
	}
	p.source.DelegateExecutable, p.updated = selected.DelegateExecutable, true
	return nil
}

var errGatewayUpgradeUnusedSDK = errors.New("unexpected SDK operation in Native Gateway upgrade fixture")

func (*gatewayUpgradeNativeProfile) Connect(context.Context, deviceclient.ConnectOptions) (deviceclient.ConnectionStatus, error) {
	return deviceclient.ConnectionStatus{}, errGatewayUpgradeUnusedSDK
}

func (*gatewayUpgradeNativeProfile) ListWorkspaces(context.Context, string) ([]deviceclient.Workspace, error) {
	return nil, errGatewayUpgradeUnusedSDK
}

func (*gatewayUpgradeNativeProfile) AllowWorkspace(context.Context, string, deviceclient.WorkspaceApproval) (deviceclient.Workspace, error) {
	return deviceclient.Workspace{}, errGatewayUpgradeUnusedSDK
}

func (*gatewayUpgradeNativeProfile) UpdateWorkspace(context.Context, string, string, uint64, deviceclient.WorkspaceUpdate) (deviceclient.Workspace, error) {
	return deviceclient.Workspace{}, errGatewayUpgradeUnusedSDK
}

func (*gatewayUpgradeNativeProfile) RemoveWorkspace(context.Context, string, string, uint64) (deviceclient.Workspace, error) {
	return deviceclient.Workspace{}, errGatewayUpgradeUnusedSDK
}

func (*gatewayUpgradeNativeProfile) OpenRuntime(context.Context, string) (gatewaycommand.Runtime, error) {
	return nil, errGatewayUpgradeUnusedSDK
}

func (*gatewayUpgradeNativeProfile) RevokeDevice(context.Context, string, func(context.Context, deviceclient.Challenge) error) error {
	return errGatewayUpgradeUnusedSDK
}

type gatewayUpgradeNativeServiceCall struct{ executable, action string }

type gatewayUpgradeNativeServices struct {
	profile string
	calls   []gatewayUpgradeNativeServiceCall
}

func (s *gatewayUpgradeNativeServices) Open(profile, executable string) (gatewaycommand.Service, error) {
	if profile != s.profile {
		return nil, task.ErrIdentityMismatch
	}
	return gatewayUpgradeNativeService{owner: s, executable: executable}, nil
}

func (s *gatewayUpgradeNativeServices) assertHandoff(t *testing.T, original, selected string) {
	t.Helper()
	if len(s.calls) < 3 {
		t.Fatalf("missing connector service handoff calls: %+v", s.calls)
	}
	last := len(s.calls) - 2
	for _, call := range s.calls[:last] {
		if call != (gatewayUpgradeNativeServiceCall{original, "uninstall"}) {
			t.Fatalf("connector source changed before Native completion: %+v", s.calls)
		}
	}
	want := []gatewayUpgradeNativeServiceCall{{selected, "install"}, {selected, "start"}}
	if !reflect.DeepEqual(s.calls[last:], want) {
		t.Fatalf("connector restart lost exact source/order: %+v", s.calls)
	}
}

type gatewayUpgradeNativeService struct {
	owner      *gatewayUpgradeNativeServices
	executable string
}

func (s gatewayUpgradeNativeService) observe(ctx context.Context, action string) (gatewayservice.Status, error) {
	if err := ctx.Err(); err != nil {
		return gatewayservice.Status{}, err
	}
	s.owner.calls = append(s.owner.calls, gatewayUpgradeNativeServiceCall{s.executable, action})
	status := gatewayservice.Status{Name: "native-upgrade-fixture", State: gatewayservice.StateInactive}
	if action == "install" || action == "start" {
		status.Installed = true
	}
	if action == "start" {
		status.State = gatewayservice.StateRunning
	}
	return status, nil
}

func (s gatewayUpgradeNativeService) Install(ctx context.Context) (gatewayservice.Status, error) {
	return s.observe(ctx, "install")
}

func (s gatewayUpgradeNativeService) Start(ctx context.Context) (gatewayservice.Status, error) {
	return s.observe(ctx, "start")
}

func (s gatewayUpgradeNativeService) Stop(ctx context.Context) (gatewayservice.Status, error) {
	return s.observe(ctx, "stop")
}

func (s gatewayUpgradeNativeService) Status(ctx context.Context) (gatewayservice.Status, error) {
	return s.observe(ctx, "status")
}

func (s gatewayUpgradeNativeService) Uninstall(ctx context.Context) (gatewayservice.Status, error) {
	return s.observe(ctx, "uninstall")
}
