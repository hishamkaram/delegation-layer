package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/migration"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Validated storage DATA only, not a claim of native birth/closure. The existing
// completion owner publishes the historical record and clears its exact Drain.
func completedInspectionUpgradeFixture(t *testing.T) (generationGuardFixture, taskdir.RootPin, privateMigrationRecord) {
	t.Helper()
	f, pin, pending := upgradeMigrationStorageFixture(t)
	completed := pending
	completed.UpgradeGeneration.AcceptedStart = true
	completed.UpgradeGeneration.SuccessorBootID = completed.UpgradeGeneration.LegacyBootID
	completed.UpgradeGeneration.SuccessorPID = 124
	completed.UpgradeGeneration.SuccessorProcessStart = "fixture-completed-successor"
	completed.UpgradeGeneration.Completed = true
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		if err := replacePrivateMigrationRecordContext(ctx, f.base, pending, completed, f.client.options.PrivateGenerationGuard); err != nil {
			return err
		}
		if err := stageUpgradePublicationInterruption(f.root, f.base, completed, "migration_removed"); err != nil {
			return err
		}
		options := f.client.options
		options.DaemonWaitOwner = &upgradeNativeWaitOwner{}
		client, observed, err := CompletePrivateUpgradeWithLock(ctx, pin, completed.UpgradeGeneration.OperationID, nil, options)
		if client != nil {
			err = errors.Join(err, client.Close())
		}
		if err == nil && !observed.Completed {
			return ErrBinding
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, pin, completed
}

func TestReadyCurrentFencesCanonicalAuthorityBeforeRecovery(t *testing.T) {
	for _, state := range []string{"current", "same publication", "completed runtime", "foreign publication", "later same target", "drain B", "initial intent", "missing publication", "valid alias", "alias with corrupt identity", "different selection", "different callback", "publication+initial intent", "publication+valid alias", "publication+pair restart", "publication+pending runtime", "publication+pending migration"} {
		t.Run(state, func(t *testing.T) { testReadyCurrentFence(t, state) })
	}
}

func TestReadyCurrentPendingARefusalDoesNotInventCompletedObservation(t *testing.T) {
	f, pin, accepted := upgradeJournalFixture(t, UpgradeOperationRequired)
	before := snapshotPrivateSuccessorTestState(t, f.root)
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		observed, err := CheckPrivateUpgradeReadyCurrentWithLock(ctx, pin, accepted.OperationID, nil, f.client.options)
		if !errors.Is(err, ErrPrivateUpgradeOperation) || observed != (PrivateUpgradeObservation{}) {
			t.Fatalf("uncompleted A escaped as a completed readiness subject: %+v %v", observed, err)
		}
		return nil
	})
	if err != nil || !reflect.DeepEqual(before, snapshotPrivateSuccessorTestState(t, f.root)) {
		t.Fatalf("ReadyCurrent changed pending A: %v", err)
	}
}

func testReadyCurrentFence(t *testing.T, state string) {
	t.Helper()
	f, pin, completed := completedInspectionUpgradeFixture(t)
	target := completed.UpgradeGeneration.Target
	// Authored finite fixture role, never copied from an installed executable.
	writeExecutable(t, target.DaemonExecutable, privateDaemonFixture)
	options := f.client.options
	selected := target
	configureReadyCurrentFence(t, f, completed, state)
	if state == "different selection" {
		selected.ClientExecutable = "/different/pueue"
	}
	if state == "different callback" {
		options.PrivateTerminalEventRunner = "/different/delegate-run"
	}
	before := snapshotPrivateSuccessorTestState(t, f.root)
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		observed, checkErr := CheckPrivateUpgradeReadyCurrentWithLock(ctx, pin, completed.UpgradeGeneration.OperationID, func(context.Context) (string, string, error) {
			return selected.ClientExecutable, selected.DaemonExecutable, nil
		}, options)
		if observed.OperationID != completed.UpgradeGeneration.OperationID || !observed.Completed {
			t.Fatalf("exact immutable A was lost: %+v %v", observed, checkErr)
		}
		return checkErr
	})
	wantReady := state == "current" || state == "same publication" || state == "completed runtime"
	if wantReady && err != nil || !wantReady && err == nil {
		t.Fatalf("current-admission pre-effect fence: state=%s err=%v", state, err)
	}
	if !reflect.DeepEqual(before, snapshotPrivateSuccessorTestState(t, f.root)) {
		t.Fatal("pre-effect fence repaired/published state or started a daemon")
	}
}

func configureReadyCurrentFence(t *testing.T, f generationGuardFixture, completed privateMigrationRecord, state string) {
	t.Helper()
	if competing, publishing := strings.CutPrefix(state, "publication+"); publishing {
		configureReadyCurrentFence(t, f, completed, "same publication")
		configureReadyCurrentFence(t, f, completed, competing)
		return
	}
	switch state {
	case "current", "different selection", "different callback":
	case "same publication", "foreign publication":
		binding := completed.UpgradeGeneration.Target
		if state == "foreign publication" {
			// Logical identity is unchanged; full executable provenance differs.
			binding.ClientExecutable = "/foreign/pueue"
		}
		writePrivateSuccessorTestRecord(t, privateIdentityPublicationPath(f.root), privateIdentityPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: binding})
	case "pair restart":
		seedInitialBootstrapControl(t, f.root, completed.UpgradeGeneration.Target, "pair-restart")
	case "pending runtime":
		record := privateRuntimeRestartRecord{From: "ready-current-old", Binding: completed.UpgradeGeneration.Target, Phase: RuntimeRestartRequested, OldPID: 101, OldProcessStart: "recorded-old"}
		writePrivateSuccessorTestRecord(t, privateBootstrapRecordPath(f.root), privateBootstrapRecord{SchemaVersion: task.SchemaVersion, RuntimeRestart: &record})
	case "pending migration":
		record := completed
		record.UpgradeGeneration = migration.UpgradeGeneration{}
		writePrivateSuccessorTestRecord(t, privateMigrationRecordPath(f.base), record)
	case "completed runtime":
		seedInitialBootstrapControl(t, f.root, completed.UpgradeGeneration.Target, "completed-runtime-restart")
	case "later same target":
		later := completed
		later.Legacy = completed.Successor
		later.UpgradeGeneration.Predecessor = completed.UpgradeGeneration.Target
		later.UpgradeGeneration.OperationID = UpgradeOperationID("upgrade-v1:" + strings.Repeat("b", 32))
		later.UpgradeGeneration.SuccessorPID++
		later.UpgradeGeneration.SuccessorProcessStart = "later-independent-birth"
		writePrivateSuccessorTestRecord(t, privateMigrationHistoryPath(f.base), privateMigrationHistory{SchemaVersion: task.SchemaVersion, Migrations: []privateMigrationRecord{completed, later}})
	case "drain B":
		pin := mustReadyCurrentPin(t, f.root)
		if err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
			_, err := BeginPrivateUpgradeWithLock(ctx, pin, UpgradeOperationRequired, f.client.options)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	case "initial intent":
		if err := markPrivateBootstrapPending(f.root); err != nil {
			t.Fatal(err)
		}
	default:
		configurePassiveInspectionIncomplete(t, f, completed, state)
	}
}

func mustReadyCurrentPin(t *testing.T, root string) taskdir.RootPin {
	t.Helper()
	pin, err := readControlRootPin(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

func TestReadyCurrentRechecksBPublishedWhileWaitingForExclusiveLease(t *testing.T) {
	f, pin, completed := completedInspectionUpgradeFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := TryWithPrivateTransition(ctx, f.root, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	launch := make(chan struct{})
	entered := make(chan struct{})
	result := make(chan error, 1)
	go func(caller context.Context) {
		select {
		case <-launch:
		case <-caller.Done():
			result <- caller.Err()
			return
		}
		_, checkErr := TryWithPrivateTransition(caller, f.root, func() error {
			close(entered)
			return WithExistingBootstrapLockContext(caller, f.root, func(locked context.Context) error {
				_, err := CheckPrivateUpgradeReadyCurrentWithLock(locked, pin, completed.UpgradeGeneration.OperationID, nil, f.client.options)
				return err
			})
		})
		result <- checkErr
	}(ctx)
	err := WithExistingBootstrapLockContext(ctx, f.root, func(locked context.Context) error {
		close(launch)
		select {
		case <-entered:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := BeginPrivateUpgradeWithLock(locked, pin, UpgradeOperationRequired, f.client.options)
		return err
	})
	if err != nil {
		cancel()
	}

	// The command is joined before any fixture cleanup, on every assertion path.
	checkErr := <-result
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(checkErr, ErrPrivateUpgradeOperation) {
		t.Fatalf("ReadyCurrent used a pre-EX observation of A after B appeared: %v", checkErr)
	}
	current, found, err := ObservePrivateUpgrade(ctx, pin, "")
	if err != nil || !found || current.OperationID == completed.UpgradeGeneration.OperationID || current.Completed {
		t.Fatalf("ReadyCurrent changed B: %+v %v", current, err)
	}
}

func TestUpgradeInspectionEligibilityUsesExactCanonicalTarget(t *testing.T) {
	f, pin, completed := completedInspectionUpgradeFixture(t)
	observed, retained, err := ObservePrivateUpgrade(t.Context(), pin, completed.UpgradeGeneration.OperationID)
	if err != nil || !retained || !observed.Completed || observed.Target != completed.UpgradeGeneration.Target {
		t.Fatalf("missing exact canonical target: %+v %v", observed, err)
	}
	data, err := json.Marshal(observed)
	if err != nil || bytes.Contains(data, []byte(`"target"`)) {
		t.Fatalf("Go-only target escaped JSON: %s %v", data, err)
	}
	err = WithSupervisorLeaseContext(t.Context(), f.root, func(ctx context.Context) error {
		target, eligible, targetErr := PrivateUpgradeInspectionTargetWithLock(ctx, pin, observed.OperationID)
		if targetErr == nil && (!eligible || target != observed.Target) {
			t.Fatalf("exact completion lost eligibility: %+v %t", target, eligible)
		}
		return targetErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = PrivateUpgradeInspectionTargetWithLock(t.Context(), pin, observed.OperationID); !errors.Is(err, ErrBinding) {
		t.Fatalf("unleased observation accepted: %v", err)
	}
}

func TestUpgradeInspectionHistoricalAWithCurrentBIsReadOnly(t *testing.T) {
	f, pin, completed := completedInspectionUpgradeFixture(t)
	var b PrivateUpgradeObservation
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		var err error
		b, err = BeginPrivateUpgradeWithLock(ctx, pin, UpgradeOperationRequired, f.client.options)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// B's unreadable evidence cannot be used or repaired while replaying A.
	if err = os.WriteFile(filepath.Join(f.base, "migration.json"), []byte("unreadable B"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotPrivateSuccessorTestState(t, f.root)
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		_, eligible, targetErr := PrivateUpgradeInspectionTargetWithLock(ctx, pin, completed.UpgradeGeneration.OperationID)
		if eligible {
			t.Fatal("historical A became current B mutation authority")
		}
		return targetErr
	})
	if err != nil || !reflect.DeepEqual(before, snapshotPrivateSuccessorTestState(t, f.root)) {
		t.Fatalf("historical A read/changed B: %v", err)
	}
	current, found, err := ObservePrivateUpgrade(t.Context(), pin, b.OperationID)
	if err != nil || !found || current.OperationID != b.OperationID || current.Completed {
		t.Fatalf("B changed: %+v %v", current, err)
	}
}

func TestUpgradeInspectionProbeRequiresAuthorityAndExactSelectedPair(t *testing.T) {
	f, pin, completed := completedInspectionUpgradeFixture(t)
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		if _, _, err := ProbePrivateUpgradeInspectionWithLock(ctx, pin, completed.UpgradeGeneration.OperationID, nil, Options{}); !errors.Is(err, ErrInfrastructureOwnerRequired) {
			t.Fatalf("missing guard granted probe: %v", err)
		}
		client, _, err := ProbePrivateUpgradeInspectionWithLock(ctx, pin, completed.UpgradeGeneration.OperationID, func(context.Context) (string, string, error) {
			return f.client.Binding().ClientExecutable, "/different/selected/pueued", nil
		}, f.client.options)
		if client != nil || !errors.Is(err, ErrBinding) {
			t.Fatalf("different installation accepted: client=%v err=%v", client, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.base, "daemon.identity.json"), []byte("corrupt current authority"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = WithSupervisorLeaseContext(t.Context(), f.root, func(ctx context.Context) error {
		_, eligible, targetErr := PrivateUpgradeInspectionTargetWithLock(ctx, pin, completed.UpgradeGeneration.OperationID)
		if eligible || targetErr == nil {
			t.Fatal("corrupt current authority became readiness")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeInspectionPassiveIncompleteDoesNotBlockExistingRecovery(t *testing.T) {
	for _, state := range []string{"valid alias", "bootstrap tail", "publication tail", "missing publication", "alias with corrupt identity"} {
		t.Run(state, func(t *testing.T) { testPassiveUpgradeInspectionIncomplete(t, state) })
	}
}

func testPassiveUpgradeInspectionIncomplete(t *testing.T, state string) {
	t.Helper()
	f, pin, completed := completedInspectionUpgradeFixture(t)
	configurePassiveInspectionIncomplete(t, f, completed, state)
	before := snapshotPrivateSuccessorTestState(t, f.root)
	err := WithSupervisorLeaseContext(t.Context(), f.root, func(ctx context.Context) error {
		for _, op := range []UpgradeOperationID{"", completed.UpgradeGeneration.OperationID} {
			_, eligible, readErr := PrivateUpgradeInspectionTargetWithLock(ctx, pin, op)
			if eligible || state == "alias with corrupt identity" && readErr == nil || state != "alias with corrupt identity" && readErr != nil {
				t.Fatalf("passive incomplete/fault distinction lost: state=%s eligible=%t err=%v", state, eligible, readErr)
			}
		}
		return nil
	})
	if err != nil || !reflect.DeepEqual(before, snapshotPrivateSuccessorTestState(t, f.root)) {
		t.Fatalf("passive recovery candidate changed journals: %v", err)
	}
}

func configurePassiveInspectionIncomplete(t *testing.T, f generationGuardFixture, completed privateMigrationRecord, state string) {
	t.Helper()
	switch state {
	case "valid alias", "alias with corrupt identity":
		client, err := NewClient(completed.UpgradeGeneration.Target, f.client.options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if closeErr := client.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
		if err = client.PersistPrivateRecoveryBindingContext(t.Context(), f.root); err != nil {
			t.Fatal(err)
		}
		if state == "alias with corrupt identity" {
			writePrivateSuccessorTestBytes(t, filepath.Join(f.base, "daemon.identity.json"), []byte("corrupt independent identity"))
		}
	case "bootstrap tail":
		if err := markPrivateBootstrapPending(f.root); err != nil {
			t.Fatal(err)
		}
	case "publication tail":
		if err := stageUpgradePublicationInterruption(f.root, f.base, completed, "pending"); err != nil {
			t.Fatal(err)
		}
	case "missing publication":
		if err := os.Remove(filepath.Join(f.base, "supervisor.identity.json")); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown incomplete state")
	}
}
