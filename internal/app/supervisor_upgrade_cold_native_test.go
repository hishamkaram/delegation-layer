package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This exercises compiled public commands, the real resident/Wait registry,
// preparation observation/guard and canonical maintenance authorizer. Only its
// scratch installation/root are owned; no provider task or fixture policy runs.
func TestSupervisorUpgradeColdPredecessorPublicCLINative(t *testing.T) {
	clientPath, daemonPath := requireRuntimeRestartCLINativeContext(t)
	f := newRuntimeRestartCLINative(t, clientPath, daemonPath)
	f.build(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	f.ensureOwner(t, ctx)
	t.Cleanup(func() { closeQuietColdUpgradeFixture(t, f) })
	originalOwner := f.preflight(t, ctx)
	binding := f.binding(t)
	original := coldUpgradeStopOriginal(t, ctx, f)

	prepared := coldUpgradeCLIStep(t, ctx, f, "", infrastructure.UpgradeAwaitingInstallation, "ensure-owner", "--prepare-upgrade")
	recovered := coldUpgradeReceiptBirth(t, ctx, f, prepared.OperationID)
	assertColdUpgradeDistinctBirth(t, original, recovered)
	completed := coldUpgradeCLIStep(t, ctx, f, prepared.OperationID, infrastructure.UpgradeMigrating,
		"ensure-owner", "--complete-upgrade", prepared.OperationID)
	if !completed.Completed {
		t.Fatal("same retained operation did not record completed handoff")
	}
	successor := f.executionTarget(t, ctx)
	assertColdUpgradeDistinctBirth(t, recovered, successor)
	if err := lifetime.WaitProcessAbsent(ctx, recovered); err != nil {
		t.Fatalf("recovered predecessor native closure unverified: %v", err)
	}
	if after := f.binding(t); after != binding {
		t.Fatal("same-installation cold upgrade rewrote the logical binding")
	}
	if current := f.preflight(t, ctx); current != originalOwner {
		t.Fatal("cold recovery silently replaced the serving resident")
	}
	coldUpgradeCLIStep(t, ctx, f, prepared.OperationID, infrastructure.UpgradeMigrating,
		"ensure-owner", "--complete-upgrade", prepared.OperationID)
	same, err := lifetime.SameProcess(successor, f.executionTarget(t, ctx))
	if err != nil || !same {
		t.Fatalf("completed operation replay started another daemon: same=%t err=%v", same, err)
	}
	t.Log("PASS: public CLI cold predecessor recovery, production resident custody/maintenance, same-operation completed handoff, original closure and replay; no provider task launched")
}

// The private receipt is a test witness, never control authority. Production
// validates its complete enclosing record. Here its exact operation and native
// birth are independently checked while the ordinary target API is fenced.
func coldUpgradeReceiptBirth(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, operation string) lifetime.ProcessRef {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, ".supervisor", "bootstrap.pending"))
	if err != nil || len(data) > task.MaxControlRecordSize {
		t.Fatalf("owned cold receipt unavailable: %v", err)
	}
	var witness struct {
		SchemaVersion int `json:"schema_version"`
		Drain         struct {
			OperationID string `json:"operation_id"`
			Start       struct {
				Version      int    `json:"version"`
				BootID       string `json:"boot_id"`
				PID          int    `json:"pid"`
				ProcessStart string `json:"process_start"`
			} `json:"predecessor_start"`
		} `json:"drain_v1"`
	}
	if err = json.Unmarshal(data, &witness); err != nil || witness.SchemaVersion != task.SchemaVersion || witness.Drain.OperationID != operation || witness.Drain.Start.Version != 1 {
		t.Fatalf("owned cold receipt identity mismatch: %v", err)
	}
	receipt, err := lifetime.NewProcessRef(witness.Drain.Start.PID, witness.Drain.Start.ProcessStart)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := lifetime.BootID()
	if err != nil || lifetime.CompareBootIDs(witness.Drain.Start.BootID, boot) != lifetime.BootSame {
		t.Fatalf("owned cold receipt boot mismatch: %v", err)
	}
	if present, observeErr := lifetime.ProcessPresent(receipt); observeErr != nil || !present {
		t.Fatalf("owned cold receipt does not identify a live native process: present=%t err=%v", present, observeErr)
	}
	if err = ctx.Err(); err != nil {
		t.Fatal(err)
	}
	return receipt
}

// The common CLI fixture requires a complete generation for ordinary daemon
// cleanup. An expected pre-Start failure can retain only a drain. Release the
// exact owned resident, require its actual process/Wait closure, and require
// native endpoint/PID absence before allowing that fixture to remove its root.
// A still-running recovery daemon keeps the resident alive and retains files.
func closeQuietColdUpgradeFixture(t *testing.T, f *runtimeRestartCLINative) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
	defer cancel()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, f.root, predicate.Default())
	if err != nil {
		t.Errorf("cold fixture cleanup metadata unavailable: %v", err)
		return
	}
	defer func() {
		if reader != nil {
			if closeErr := reader.Close(); closeErr != nil {
				t.Errorf("cold fixture cleanup reader close: %v", closeErr)
			}
		}
	}()
	pin, err := reader.PinRootContext(ctx)
	if err != nil {
		t.Errorf("cold fixture cleanup pin: %v", err)
		return
	}
	_, pending, err := pueue.ObservePrivateUpgrade(ctx, pin, "")
	if err != nil || !pending {
		return // Complete generations retain the common fixture's full cleanup.
	}
	if len(f.startups) != 1 {
		t.Error("pending cold fixture has an unclassified owner inventory; retaining root")
		return
	}
	stopped, err := infrastructure.StopOwnerServingAt(ctx, reader, f.startups[0])
	if err != nil || stopped.State != infrastructure.OwnerServingReleased {
		t.Errorf("pending cold fixture owner release: %v", err)
		return
	}
	closed, err := infrastructure.WaitOwnerProcessClosed(ctx, reader, f.startups[0])
	if err != nil || closed.ProcessClosure != infrastructure.OwnerProcessClosed {
		t.Errorf("pending cold fixture actual Wait owner closure: %v", err)
		return
	}
	for _, name := range []string{"pueue.pid", "pueue.sock"} {
		if _, statErr := os.Lstat(filepath.Join(f.root, ".supervisor", "run", name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("pending cold fixture retained native endpoint metadata: %s", name)
			return
		}
	}
	closeErr := reader.Close()
	reader = nil
	if closeErr != nil {
		t.Errorf("cold fixture cleanup reader close: %v", closeErr)
		return
	}
	f.started = false // Exact process and its owned Waits are proven closed.
}

func coldUpgradeStopOriginal(t *testing.T, ctx context.Context, f *runtimeRestartCLINative) lifetime.ProcessRef {
	t.Helper()
	target, err := pueue.ObservePrivateExecutionTarget(ctx, f.root, pueue.Options{Environment: f.environment})
	if err != nil || target.State() != pueue.ExecutionTargetRunning {
		t.Fatalf("original daemon target unverified: state=%v err=%v", target.State(), err)
	}
	original, err := lifetime.NewProcessRef(target.PID(), target.StartToken())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.shutdownDaemon(ctx, target); err != nil {
		t.Fatalf("canonical original daemon shutdown/closure failed: %v", err)
	}
	return original
}

func assertColdUpgradeDistinctBirth(t *testing.T, before, after lifetime.ProcessRef) {
	t.Helper()
	same, err := lifetime.SameProcess(before, after)
	if err != nil || same {
		t.Fatalf("cold upgrade did not produce a fresh native birth: same=%t err=%v", same, err)
	}
}

// Reconcile only explicit pending progress from this exact public operation.
// Readiness loss, unknown birth or another refusal is never a retry permission.
func coldUpgradeCLIStep(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, operation string, phase infrastructure.UpgradePhase, arguments ...string) SupervisorUpgradeResponse {
	t.Helper()
	for range 4 {
		response, code, err := f.command(ctx, arguments...)
		control := response.SupervisorControl
		if err != nil || response.RootID != f.rootID || control == nil || control.Upgrade == nil {
			logColdUpgradeFailure(t, control)
			t.Fatalf("public cold upgrade response unavailable: exit=%d err=%v", code, err)
		}
		view := *control.Upgrade
		if operation == "" {
			operation = view.OperationID
		}
		if _, parseErr := infrastructure.ParseUpgradeOperationID(view.OperationID); parseErr != nil || view.OperationID != operation {
			t.Fatalf("public cold upgrade changed operation: %v", parseErr)
		}
		if code == 0 && view.Phase == string(phase) {
			return view
		}
		if code != 3 || control.NextAction != "repeat_supervisor_ensure_owner" || !coldUpgradeRepeatableReason(control.ReasonCode) {
			t.Fatalf("public cold upgrade blocked: exit=%d phase=%s reason=%s next=%s", code, view.Phase, control.ReasonCode, control.NextAction)
		}
	}
	t.Fatal("same public cold-upgrade operation remained pending within its finite workflow")
	return SupervisorUpgradeResponse{}
}

func logColdUpgradeFailure(t *testing.T, control *SupervisorControlResponse) {
	t.Helper()
	if control == nil {
		return
	}
	var operation, upgradePhase string
	if control.Upgrade != nil {
		operation = control.Upgrade.OperationID
		upgradePhase = control.Upgrade.Phase
	}
	t.Logf("public cold upgrade failure facts: state=%q migration_phase=%q upgrade_phase=%q reason=%q next=%q operation=%q", control.State, control.MigrationPhase, upgradePhase, control.ReasonCode, control.NextAction, operation)
}

func coldUpgradeRepeatableReason(reason string) bool {
	return reason == "predecessor_custody_unavailable" || reason == "predecessor_wait_pending"
}
