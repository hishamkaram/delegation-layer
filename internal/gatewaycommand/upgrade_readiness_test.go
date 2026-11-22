package gatewaycommand

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestGatewayUpgradeFinalReadinessIsExplicitAndPassive(t *testing.T) {
	for _, state := range []string{"ready", "migrated", "not_initialized", "legacy_present", "unavailable", "unknown"} {
		t.Run(state, func(t *testing.T) {
			native, runner := upgradePublicFixture(t)
			binding := runner.binding
			if state == "not_initialized" {
				binding.Relation = installation.BindingNotInitialized
			}
			runner.readiness = &nativeSupervisorControl{State: state, NextAction: "none", InstallationBinding: &binding}
			if state == "legacy_present" {
				runner.readiness.NextAction = "run_supervisor_migrate"
			}
			if err := native.Verify(boundedTestContext(t), runner.root, runner.rootToken); err != nil {
				t.Fatalf("readiness blocked initial capability verification: %v", err)
			}
			err := native.VerifyReady(boundedTestContext(t), runner.root, runner.rootToken)
			wantReady := state == "ready" || state == "migrated" || state == "not_initialized"
			if wantReady && err != nil || !wantReady && !errors.Is(err, ErrInstallationMigrationRequired) {
				t.Fatalf("readiness state=%s err=%v", state, err)
			}
		})
	}
}

func TestGatewayUpgradeFinalReadinessRejectsMissingOrForeignStatusRoot(t *testing.T) {
	for _, rootID := range []string{"", strings.Repeat("f", 32)} {
		native, runner := upgradePublicFixture(t)
		runner.statusRootID = &rootID
		if err := native.Verify(boundedTestContext(t), runner.root, runner.rootToken); err != nil {
			t.Fatal(err)
		}
		if err := native.VerifyReady(boundedTestContext(t), runner.root, runner.rootToken); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("foreign/missing status root accepted: root=%q err=%v", rootID, err)
		}
	}
}

func TestGatewayUpgradeFinalObservationRetryRetainsNoNewAuthority(t *testing.T) {
	for _, change := range []string{"prerequisite corrected", "unrelated B appeared", "handoff now required"} {
		t.Run(change, func(t *testing.T) { testGatewayReadinessRetry(t, change) })
	}
}

func testGatewayReadinessRetry(t *testing.T, change string) {
	t.Helper()
	f := newUpgradeFixture(t)
	f.native.readyErr = task.ErrIdentityMismatch
	first, code := f.run(t, false)
	assertSuccessfulNoOperationReadinessFailure(t, f, first, code)
	f.native.readyErr = nil
	switch change {
	case "unrelated B appeared":
		f.native.transition = NativeTransition{OperationID: "upgrade-v1:" + strings.Repeat("b", 32), Phase: "draining", Pending: true}
	case "handoff now required":
		f.native.transition = NativeTransition{HandoffNeeded: true}
	case "prerequisite corrected":
	default:
		t.Fatal("unknown retry partition")
	}
	second, code := f.run(t, false)
	if f.native.ensures != 1 {
		t.Fatal("observation retry acquired Native mutation authority")
	}
	if change == "prerequisite corrected" {
		if code != 0 || second.Upgrade.Phase != "complete" {
			t.Fatalf("corrected final observation remained permanently unknown: code=%d response=%+v", code, second)
		}
	} else if code == 0 || second.Upgrade.ServiceRestarted {
		t.Fatalf("reduced-authority retry acted on changed Native state: code=%d response=%+v", code, second)
	}
	assertReadinessRetryPins(t, f)
	f.assertPreserved(t)
}

func TestGatewayUpgradeCompletedPrivateOperationCanRetainReadinessPending(t *testing.T) {
	f := newUpgradeFixture(t)
	operation := "upgrade-v1:" + strings.Repeat("a", 32)
	f.native.transition = NativeTransition{OperationID: operation, Phase: "migrating", HandoffNeeded: true}
	f.native.ensureErr = ErrNativeUpgradePending
	response, code := f.run(t, false)
	if code != 3 || response.Failure != "gateway_upgrade_pending" || response.Upgrade.Supervisor.OperationID != operation || response.Upgrade.Supervisor.Pending || f.native.ensures != 1 {
		t.Fatalf("readiness pending falsified private completion: code=%d response=%+v", code, response)
	}
	source, err := f.client.ProfileMigrationSource(boundedTestContext(t), f.profile)
	if err != nil || source.DelegateExecutable != f.oldExecutable || len(f.services.calls) != 1 || f.services.calls[0].action != "uninstall" {
		t.Fatalf("readiness pending committed/resumed connector: source=%+v err=%v", source, err)
	}
	assertGUICompletedReadinessPending(t, f, operation)
	f.assertPreserved(t)
}

func assertGUICompletedReadinessPending(t *testing.T, f *upgradeFixture, operation string) {
	t.Helper()
	directory := filepath.Join(f.profile, ".gui-upgrade", f.gui.requests[0].Invocation)
	for _, name := range []string{"joined", "cleaned"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(directory, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result gatewayservice.GUIUpgradeResult
	if err = json.Unmarshal(data, &result); err != nil || result.OperationID != operation || result.Pending || result.ExitCode != 3 {
		t.Fatalf("GUI receipt lost exact completed A/readiness pending: %+v %v", result, err)
	}
}

func TestGatewayUpgradeReadinessFailurePreventsSDKCommitAndServiceResume(t *testing.T) {
	f := newUpgradeFixture(t)
	profilePath := filepath.Join(f.profile, "profile.json")
	before, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	f.native.readyErr = ErrInstallationMigrationRequired
	response, code := f.run(t, false)
	if code == 0 || response.Upgrade == nil || response.Upgrade.Phase != "verify_current_installation" || response.Upgrade.ServiceRestarted {
		t.Fatalf("unready Native reached commit/resume: exit=%d response=%+v", code, response.Upgrade)
	}
	after, err := os.ReadFile(profilePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("readiness failure changed SDK profile: %v", err)
	}
	for _, call := range f.services.calls {
		if call.action == "install" || call.action == "start" {
			t.Fatalf("readiness failure resumed connector: %+v", f.services.calls)
		}
	}
	f.assertPreserved(t)
}

func assertSuccessfulNoOperationReadinessFailure(t *testing.T, f *upgradeFixture, first Response, code int) {
	t.Helper()
	if code == 0 || first.Upgrade.Phase != "verify_current_installation" || first.Upgrade.Supervisor != (NativeTransition{}) || first.Upgrade.Native.RootContextMatches || first.Upgrade.Native.CapabilitiesVerified || f.native.ensures != 1 {
		t.Fatalf("fixture did not isolate final successful no-operation observation: code=%d response=%+v", code, first.Upgrade)
	}
}

func assertReadinessRetryPins(t *testing.T, f *upgradeFixture) {
	t.Helper()
	if len(f.gui.requests) != 2 || !f.gui.requests[1].NoNewNativeOperation || f.gui.requests[1].ExpectedOperation != "" || f.gui.requests[1].PATHSource != "prior_receipt" || f.gui.requests[1].Executable != f.newExecutable || f.gui.requests[1].SavedSource.Executable != f.oldExecutable {
		t.Fatal("explicit retry lost saved source/target/PATH or authority reduction")
	}
}

func TestGatewayUpgradeEmptyMaintenancePendingCanResumeNormalIntent(t *testing.T) {
	for _, next := range []string{"ordinary readiness", "compatible B", "incompatible B"} {
		t.Run(next, func(t *testing.T) { testEmptyMaintenancePendingRetry(t, next) })
	}
}

func testEmptyMaintenancePendingRetry(t *testing.T, next string) {
	t.Helper()
	f := newUpgradeFixture(t)
	f.native.ensureErr = ErrNativeUpgradePending
	first, code := f.run(t, false)
	assertEmptyMaintenancePending(t, f, first, code)
	f.native.ensureErr = nil
	operation := ""
	if next != "ordinary readiness" {
		operation = "upgrade-v1:" + strings.Repeat("b", 32)
		f.native.transition = NativeTransition{OperationID: operation, Phase: "awaiting_installation", Pending: true}
	}
	if next == "incompatible B" {
		f.native.ensureErr = task.ErrRequestConflict
	}
	second, code := f.run(t, false)
	if f.native.ensures != 2 {
		t.Fatalf("new explicit normal intent stayed permanently unknown: ensures=%d exit=%d response=%+v", f.native.ensures, code, second)
	}
	if first.Upgrade.Supervisor != (NativeTransition{}) {
		t.Fatal("later B was attributed to the earlier empty response")
	}
	if next == "incompatible B" {
		if code == 0 || second.Upgrade.ServiceRestarted {
			t.Fatal("incompatible B committed/resumed connector")
		}
		source, err := f.client.ProfileMigrationSource(boundedTestContext(t), f.profile)
		if err != nil || source.DelegateExecutable != f.oldExecutable {
			t.Fatalf("incompatible B changed SDK selection: %v", err)
		}
	} else if code != 0 || second.Upgrade.Supervisor.OperationID != operation {
		t.Fatalf("explicit normal retry did not complete its initial subject: exit=%d response=%+v", code, second)
	}
	assertNormalMaintenanceRetryPins(t, f)
	f.assertPreserved(t)
}

func assertEmptyMaintenancePending(t *testing.T, f *upgradeFixture, first Response, code int) {
	t.Helper()
	if code != 3 || first.Failure != "gateway_upgrade_pending" || first.Upgrade.Supervisor != (NativeTransition{}) || f.native.ensures != 1 {
		t.Fatalf("empty maintenance pending lost its truthful public projection: exit=%d response=%+v", code, first)
	}
	assertGUICompletedReadinessPending(t, f, "") // Actual joined/cleaned Manager receipt.
	if first.Upgrade.Phase != "supervisor_maintenance_pending" || first.Upgrade.Native.RootContextMatches || first.Upgrade.Native.CapabilitiesVerified {
		t.Fatal("typed normal pending became positive no-operation verification")
	}
}

func TestGatewayUpgradeAmbiguousMaintenanceDoesNotGainNormalRetry(t *testing.T) {
	for _, kind := range []string{"mixed fault", "canceled", "expired", "review required", "selected A", "returned handoff"} {
		t.Run(kind, func(t *testing.T) {
			f := newUpgradeFixture(t)
			configureAmbiguousMaintenance(t, f, kind)
			first, code := f.run(t, false)
			if code == 0 || first.Upgrade.Phase == "supervisor_maintenance_pending" {
				t.Fatalf("ambiguous outcome gained normal retry: kind=%s response=%+v", kind, first)
			}
			f.native.ensureErr = nil
			if second, retryCode := f.run(t, false); retryCode == 0 || f.native.ensures != 1 || second.Upgrade.ServiceRestarted {
				t.Fatalf("ambiguous maintenance relaunched: kind=%s ensures=%d exit=%d", kind, f.native.ensures, retryCode)
			}
			f.assertPreserved(t)
		})
	}
}

func configureAmbiguousMaintenance(t *testing.T, f *upgradeFixture, kind string) {
	t.Helper()
	f.native.ensureErr = ErrNativeUpgradePending
	switch kind {
	case "mixed fault":
		f.native.ensureErr = errors.Join(ErrNativeUpgradePending, task.ErrEvidenceFault)
	case "canceled":
		f.native.ensureErr = errors.Join(ErrNativeUpgradePending, context.Canceled)
	case "expired":
		f.native.ensureErr = errors.Join(ErrNativeUpgradePending, context.DeadlineExceeded)
	case "review required":
		f.native.ensureErr = ErrNativeUpgradeReviewRequired
	case "selected A":
		f.native.transition = NativeTransition{OperationID: "upgrade-v1:" + strings.Repeat("a", 32), Phase: "draining", Pending: true}
		f.native.returned = &NativeTransition{}
	case "returned handoff":
		f.native.returned = &NativeTransition{HandoffNeeded: true}
	}
}

func assertNormalMaintenanceRetryPins(t *testing.T, f *upgradeFixture) {
	t.Helper()
	if len(f.gui.requests) != 2 || f.gui.requests[1].ExpectedOperation != "" || f.gui.requests[1].NoNewNativeOperation || f.gui.requests[1].SavedSource != f.gui.requests[0].SavedSource || f.gui.requests[1].PATHSource != "prior_receipt" {
		t.Fatal("pending retry lost source/PATH or changed original normal authority")
	}
}
