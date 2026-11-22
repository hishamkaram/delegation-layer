package gatewaycommand

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type upgradeClient struct {
	sdkClient
	fault       string
	active      bool
	inactiveErr error
}

func (c *upgradeClient) RuntimeInactive(ctx context.Context, p string) (bool, error) {
	if c.inactiveErr != nil {
		return false, c.inactiveErr
	}
	if c.active {
		return false, nil
	}
	return c.sdkClient.RuntimeInactive(ctx, p)
}

func (c *upgradeClient) MigrateProfile(ctx context.Context, o deviceclient.ProfileMigrationOptions) (deviceclient.ProfileMigrationPreview, error) {
	preview, err := c.sdkClient.MigrateProfile(ctx, o)
	if err == nil && c.fault == "after_profile_commit" {
		c.fault = ""
		return preview, errors.Join(deviceclient.ErrPersistence, errUpgradeFixture)
	}
	return preview, err
}

func (c *upgradeClient) UpdateInstallationSelection(ctx context.Context, p string, expected, selected deviceclient.InstallationSelection) error {
	if c.fault == "before_selection_commit" {
		c.fault = ""
		return errors.Join(deviceclient.ErrPersistence, errUpgradeFixture)
	}
	err := c.sdkClient.UpdateInstallationSelection(ctx, p, expected, selected)
	if err == nil && c.fault == "after_selection_commit" {
		c.fault = ""
		return errors.Join(deviceclient.ErrPersistence, errUpgradeFixture)
	}
	return err
}

type upgradeNative struct {
	root, rootToken                     string
	transition                          NativeTransition
	inspections, ensures, verifications int
	verifyErr                           error
	ensureErr                           error
	keepPending                         bool
}

func (n *upgradeNative) Verify(ctx context.Context, root, expected string) error {
	n.verifications++
	if n.verifyErr != nil {
		return n.verifyErr
	}
	if root != n.root || expected != n.rootToken {
		return task.ErrIdentityMismatch
	}
	_, current, err := rootContext(ctx, root, predicate.Default(), false)
	if err != nil || current.RootToken != expected {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	return nil
}

func (n *upgradeNative) Inspect(_ context.Context, root, expected string) (NativeTransition, error) {
	n.inspections++
	if root != n.root || expected != n.rootToken {
		return NativeTransition{}, task.ErrIdentityMismatch
	}
	return n.transition, nil
}

func (n *upgradeNative) Ensure(_ context.Context, root, expected string, previous NativeTransition) (NativeTransition, error) {
	n.ensures++
	if root != n.root || expected != n.rootToken || previous != n.transition {
		return previous, task.ErrIdentityMismatch
	}
	if n.ensureErr != nil {
		return previous, n.ensureErr
	}
	if n.keepPending {
		return previous, ErrNativeUpgradePending
	}
	n.transition.Pending, n.transition.HandoffNeeded = false, false
	return n.transition, nil
}

type (
	upgradeServiceCall struct{ executable, action string }
	upgradeServices    struct {
		calls     []upgradeServiceCall
		installed string
		fault     string
	}
)

func (s *upgradeServices) Open(_ string, executable string) (Service, error) {
	return upgradeService{owner: s, executable: executable}, nil
}

type upgradeService struct {
	owner      *upgradeServices
	executable string
}

func (s upgradeService) observe(action string) (gatewayservice.Status, error) {
	s.owner.calls = append(s.owner.calls, upgradeServiceCall{s.executable, action})
	status := gatewayservice.Status{Name: "same-profile", Installed: s.owner.installed != "", State: gatewayservice.StateInactive}
	if s.owner.fault == action {
		s.owner.fault = ""
		return status, gatewayservice.ErrManager
	}
	switch action {
	case "uninstall":
		s.owner.installed = ""
		status.Installed = false
	case "install":
		if s.owner.installed != "" && s.owner.installed != s.executable {
			return status, gatewayservice.ErrForeignService
		}
		s.owner.installed = s.executable
		status.Installed = true
	case "start":
		if s.owner.installed != s.executable {
			return status, gatewayservice.ErrForeignService
		}
		status.State = gatewayservice.StateRunning
		if s.owner.fault == "unknown_start_state" {
			s.owner.fault = ""
			status.State = gatewayservice.StateUnknown
		}
	}
	return status, nil
}

func (s upgradeService) Install(context.Context) (gatewayservice.Status, error) {
	return s.observe("install")
}

func (s upgradeService) Start(context.Context) (gatewayservice.Status, error) {
	return s.observe("start")
}

func (s upgradeService) Stop(context.Context) (gatewayservice.Status, error) {
	return s.observe("stop")
}

func (s upgradeService) Status(context.Context) (gatewayservice.Status, error) {
	return s.observe("status")
}

func (s upgradeService) Uninstall(context.Context) (gatewayservice.Status, error) {
	return s.observe("uninstall")
}

func TestGatewayUpgradePreviewIsMetadataOnlyWithRemovedOldKeg(t *testing.T) {
	f := newUpgradeFixture(t)
	if err := os.Remove(f.oldExecutable); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.profile, "profile.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	response, code := f.run(t, true)
	if code != 0 || response.Upgrade == nil || response.Upgrade.Phase != "preview" {
		t.Fatalf("preview failed: exit%d %+v", code, response)
	}
	requireUpgradePreviewMetadata(t, f, response.Upgrade)
	if len(f.services.calls) != 0 || f.native.inspections != 0 || f.native.verifications != 0 || f.native.ensures != 0 {
		t.Fatal("preview invoked a service or Native command")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("preview rewrote profile: %v", err)
	}
	f.assertPreserved(t)
}

func TestGatewayUpgradeUsesNewInstalledExecutableAndPreservesOriginalState(t *testing.T) {
	f := newUpgradeFixture(t)
	if err := os.Remove(f.oldExecutable); err != nil {
		t.Fatal(err)
	}
	f.services.installed = f.oldExecutable
	response, code := f.run(t, false)
	requireCompletedGatewayUpgrade(t, response, code)
	if f.native.ensures != 0 || f.native.verifications != 1 {
		t.Fatal("new connector executable invented a supervisor operation or repeated unchanged preflight")
	}
	expectedCalls := []upgradeServiceCall{{f.oldExecutable, "uninstall"}, {f.newExecutable, "install"}, {f.newExecutable, "start"}}
	if !reflect.DeepEqual(f.services.calls, expectedCalls) {
		t.Fatalf("source handoff changed service order: %+v", f.services.calls)
	}
	source, err := deviceclient.LocalContext(t.Context(), f.profile)
	if err != nil || source.DelegateExecutable != f.newExecutable || source.DelegateRoot != f.root || source.DelegateRootContext != f.rootToken {
		t.Fatalf("installation selection did not preserve root: %v", err)
	}
	workspaces, err := deviceclient.ListWorkspaces(t.Context(), f.profile)
	if err != nil || len(workspaces) != 1 || workspaces[0].Enabled || workspaces[0].Revision != 2 {
		t.Fatalf("upgrade rewrote disabled workspace history: %v", err)
	}
	f.assertPreserved(t)
}

func TestGatewayUpgradeResumesCommittedProfileAndSelectionAfterLostAcknowledgment(t *testing.T) {
	for _, stage := range []string{"after_profile_commit", "before_selection_commit", "after_selection_commit"} {
		t.Run(stage, func(t *testing.T) {
			f := newUpgradeFixture(t)
			f.client.fault = stage
			failed, code := f.run(t, false)
			if code == 0 || failed.Upgrade == nil || failed.Upgrade.ServiceRestarted {
				t.Fatalf("lost acknowledgment became successful handoff: exit%d %+v", code, failed)
			}
			f.assertPreserved(t)
			completed, code := f.run(t, false)
			if code != 0 || completed.Upgrade == nil || completed.Upgrade.Phase != "complete" || !completed.Upgrade.DeviceIdentityPreserved {
				t.Fatalf("retry did not resume committed selection: exit%d %+v", code, completed)
			}
			f.assertPreserved(t)
		})
	}
}

func TestGatewayUpgradeResumesOnlyObservedSupervisorOperation(t *testing.T) {
	f := newUpgradeFixture(t)
	op := "upgrade-v1:" + strings.Repeat("a", 32)
	f.native.transition = NativeTransition{OperationID: op, Phase: "awaiting_installation", Pending: true}
	f.native.keepPending = true
	before, err := os.ReadFile(filepath.Join(f.profile, "profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	response, code := f.run(t, false)
	if code != 3 || response.Upgrade == nil || response.Upgrade.Supervisor.OperationID != op || response.Upgrade.ServiceRestarted {
		t.Fatalf("pending Native operation lost identity: exit%d %+v", code, response)
	}
	after, err := os.ReadFile(filepath.Join(f.profile, "profile.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("pending supervisor migration changed profile")
	}
	f.native.keepPending = false
	response, code = f.run(t, false)
	if code != 0 || response.Upgrade == nil || response.Upgrade.Supervisor.OperationID != op || f.native.ensures != 2 {
		t.Fatalf("retry replaced or skipped exact supervisor operation: exit%d %+v", code, response)
	}
	f.assertPreserved(t)
}

func TestGatewayUpgradeFailedQuiescenceOrVerificationPreservesOldProfile(t *testing.T) {
	for _, scenario := range []string{"service_stop", "runtime_busy", "runtime_unknown", "incompatible_native"} {
		t.Run(scenario, func(t *testing.T) {
			f := newUpgradeFixture(t)
			wantedCode := 1
			switch scenario {
			case "service_stop":
				f.services.fault = "uninstall"
			case "runtime_busy":
				f.client.active = true
				wantedCode = 3
			case "runtime_unknown":
				f.client.inactiveErr = os.ErrPermission
			case "incompatible_native":
				f.native.verifyErr = ErrInstallationMigrationRequired
			}
			path := filepath.Join(f.profile, "profile.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			response, code := f.run(t, false)
			if code != wantedCode || response.Upgrade == nil || response.Upgrade.ServiceRestarted || f.native.ensures != 0 {
				t.Fatalf("failed precondition advanced handoff: exit%d %+v", code, response)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed precondition changed original profile")
			}
			f.assertPreserved(t)
		})
	}
}

func TestGatewayUpgradeServiceFailureRetainsCommittedSelectionForRetry(t *testing.T) {
	for _, stage := range []string{"install", "start", "unknown_start_state"} {
		t.Run(stage, func(t *testing.T) {
			f := newUpgradeFixture(t)
			f.services.fault = stage
			failed, code := f.run(t, false)
			phase := stage + "_service"
			if stage == "unknown_start_state" {
				phase = "start_service"
			}
			if code != 1 || failed.Upgrade == nil || failed.Upgrade.ServiceRestarted || failed.Upgrade.Phase != phase {
				t.Fatalf("service failure lost phase: exit%d %+v", code, failed)
			}
			source, err := deviceclient.LocalContext(t.Context(), f.profile)
			if err != nil || source.DelegateExecutable != f.newExecutable {
				t.Fatalf("service failure rolled back committed source: %v", err)
			}
			f.assertPreserved(t)
			completed, code := f.run(t, false)
			requireCompletedGatewayUpgrade(t, completed, code)
			f.assertPreserved(t)
		})
	}
}

func TestGatewayOldProfileStatusAndServiceStartGuideExplicitUpgrade(t *testing.T) {
	f := newUpgradeFixture(t)
	for _, action := range []Arguments{{Action: "status"}, {Action: "serve"}, {Action: "service", ServiceAction: "start"}} {
		var output, diagnostic bytes.Buffer
		code := Run(boundedTestContext(t), action, f.root, true, &output, &diagnostic, f.dependencies)
		if code != 1 || !bytes.Contains(output.Bytes(), []byte(`"failure":"migration_required"`)) || !bytes.Contains(output.Bytes(), []byte(`"next_action":"run_gateway_upgrade_preview"`)) {
			t.Fatalf("old %s did not guide upgrade: exit%d %s", action.Action, code, output.String())
		}
	}
	if len(f.services.calls) != 0 {
		t.Fatal("old profile started a service")
	}
	f.assertPreserved(t)
}

func TestGatewayCurrentProfileWithMissingExecutableGuidesUpgrade(t *testing.T) {
	f := newUpgradeFixture(t)
	if _, err := deviceclient.MigrateProfile(t.Context(), deviceclient.ProfileMigrationOptions{ProfileDirectory: f.profile, AcceptExecutionPolicyRemoval: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.oldExecutable); err != nil {
		t.Fatal(err)
	}
	var output, diagnostic bytes.Buffer
	code := Run(boundedTestContext(t), Arguments{Action: "status"}, f.root, true, &output, &diagnostic, f.dependencies)
	if code != 1 || !bytes.Contains(output.Bytes(), []byte(`"failure":"migration_required"`)) || !bytes.Contains(output.Bytes(), []byte(`"online":"unknown"`)) {
		t.Fatalf("missing selected executable claimed readiness: exit%d %s", code, output.String())
	}
	f.assertPreserved(t)
}

func requireUpgradePreviewMetadata(t *testing.T, f *upgradeFixture, preview *UpgradeObservation) {
	t.Helper()
	if preview.Profile.FromVersion != 2 || preview.Profile.ToVersion != 3 || preview.Profile.Workspaces != 1 || preview.Profile.DisabledWorkspaces != 1 || preview.Profile.ExecutionPolicies != 2 {
		t.Fatalf("preview counts changed: %+v", preview.Profile)
	}
	if preview.Native.SavedExecutable != f.oldExecutable || preview.Native.SelectedExecutable != f.newExecutable || preview.Native.NativeRoot != f.root {
		t.Fatal("preview lost exact local source selection")
	}
}

func requireCompletedGatewayUpgrade(t *testing.T, response Response, code int) {
	t.Helper()
	if code != 0 || response.Upgrade == nil || response.Upgrade.Phase != "complete" || !response.Upgrade.DeviceIdentityPreserved || !response.Upgrade.ServiceRestarted {
		t.Fatalf("upgrade failed: exit%d %+v", code, response)
	}
	if response.Online != "unknown" || response.ExecutionReadiness != "unknown" {
		t.Fatal("service restart was presented as remote readiness")
	}
}
