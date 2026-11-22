package infrastructurehost

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestUpgradeCompletionObserverLetsExactDrainReachOwner(t *testing.T) {
	f := newInfrastructureSavedBindingFixture(t, infrastructure.SavedBindingTaskMetadata, true)
	pin, err := f.store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(f.store.Root, ".supervisor")
	if err = os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	op := infrastructure.UpgradeOperationID("upgrade-v1:" + strings.Repeat("9", 32))
	data, err := task.MarshalCanonical(map[string]any{"schema_version": task.SchemaVersion, "drain_v1": map[string]any{"root": map[string]any{"path": pin.Root(), "id": pin.RootID(), "device": pin.Device(), "inode": pin.Inode()}, "predecessor": f.binding, "operation_id": op, "mode": "operation_required", "phase": "draining"}})
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(base, "bootstrap.pending"), data)
	observed, terminal, err := observeUpgradeCompletion(t.Context(), pin, op)
	if err != nil || terminal || observed.OperationID != pueue.UpgradeOperationID(op) || observed.Phase != pueue.UpgradeDraining {
		t.Fatalf("exact draining completion was short-circuited before its owner: terminal=%t observed=%+v err=%v", terminal, observed, err)
	}
}

func TestExactCompletionAdvancesOnlyItsDrainAndPreservesMode(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mode          pueue.UpgradeMode
		advanceBefore bool
	}{
		{"reactive draining", pueue.UpgradeReactive, false},
		{"proactive draining", pueue.UpgradeOperationRequired, false},
		{"proactive awaiting installation", pueue.UpgradeOperationRequired, true},
	} {
		t.Run(tc.name, func(t *testing.T) { exerciseExactCompletionAuthorization(t, tc.mode, tc.advanceBefore) })
	}
}

func exerciseExactCompletionAuthorization(t *testing.T, mode pueue.UpgradeMode, advanceBefore bool) {
	t.Helper()
	store, original, owner := newOwnedPreparationProbe(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	pin, err := store.PinRootContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	authorizations := 0
	options := pueue.Options{
		ObservationTimeout: 10 * time.Second, WaitForPendingClose: true, PrivateGenerationGuard: owner, DaemonWaitOwner: owner, DaemonStartGuard: owner,
		PrivateMigrationAuthorizer: func(context.Context, string, task.SupervisorRef, []pueue.Job) error { authorizations++; return nil },
	}
	preparation := infrastructurePreparationOwner{store: store, options: options, lookup: installation.ExecutableLookup{InitialExecutable: filepath.Join(owner.base, "missing-pueue")}}
	var operation pueue.UpgradeOperationID
	base := filepath.Dir(original.Binding().ConfigPath)
	before := supervisorfixture.CapturePairFaultWitness(t, base)
	err = pueue.WithExistingBootstrapLockContext(ctx, store.Root, func(locked context.Context) error {
		var beginErr error
		operation, beginErr = beginExactCompletionFixture(locked, pin, mode, advanceBefore, options)
		if beginErr != nil {
			return beginErr
		}
		observed, completeErr := preparation.upgradeLocked(locked, pin, infrastructure.UpgradeRequest{Action: infrastructure.UpgradeComplete, OperationID: infrastructure.UpgradeOperationID(operation)})
		if completeErr == nil || observed.OperationID != operation || observed.Phase != pueue.UpgradeAuthorized {
			t.Errorf("completion did not authorize its exact drain before missing target refusal: %+v %v", observed, completeErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observed, found, err := pueue.ObservePrivateUpgrade(ctx, pin, operation)
	if err != nil || !found || observed.Mode != mode || observed.Phase != pueue.UpgradeAuthorized || authorizations != 1 {
		t.Fatalf("exact completion changed mode or skipped drain policy: %+v found=%t calls=%d err=%v", observed, found, authorizations, err)
	}
	assertExactCompletionAuthorization(t, base, mode, operation)
	after := supervisorfixture.CapturePairFaultWitness(t, base)
	// Only the exact drain/authorization record may change before the
	// unavailable target is refused. Pair, migration and Start stay fixed.
	delete(before, "bootstrap.pending")
	delete(after, "bootstrap.pending")
	if !maps.Equal(before, after) {
		t.Fatal("authorization changed pair, migration or Start evidence")
	}
}

func beginExactCompletionFixture(ctx context.Context, pin taskdir.RootPin, mode pueue.UpgradeMode, advanceBefore bool, options pueue.Options) (pueue.UpgradeOperationID, error) {
	begun, err := pueue.BeginPrivateUpgradeWithLock(ctx, pin, mode, options)
	if err != nil || !advanceBefore {
		return begun.OperationID, err
	}
	awaiting, err := pueue.AdvancePrivateUpgradeDrainWithLock(ctx, pin, begun.OperationID, options)
	if err == nil && awaiting.Phase != pueue.UpgradeAwaitingInstallation {
		err = errors.New("fixture did not reach awaiting installation")
	}
	return begun.OperationID, err
}

func assertExactCompletionAuthorization(t *testing.T, base string, mode pueue.UpgradeMode, operation pueue.UpgradeOperationID) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(base, "bootstrap.pending"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Drain struct {
			Authorization *struct {
				OperationID pueue.UpgradeOperationID `json:"operation_id"`
			} `json:"completion_authorization"`
		} `json:"drain_v1"`
	}
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if mode == pueue.UpgradeOperationRequired && (record.Drain.Authorization == nil || record.Drain.Authorization.OperationID != operation) {
		t.Fatal("proactive completion lost its explicit authorization")
	}
	if mode == pueue.UpgradeReactive && record.Drain.Authorization != nil {
		t.Fatal("reactive completion invented proactive authority")
	}
}

func TestCleanDrainIncompleteRejectsIndependentPendingAndFaults(t *testing.T) {
	pending := &pueue.PrivateUpgradeError{ReasonCode: "drain_incomplete", Err: pueue.ErrPrivateUpgradePending}
	for _, tc := range []struct {
		name  string
		cause error
		want  bool
	}{
		{"clean", pending, true},
		{"canceled", errors.Join(pending, context.Canceled), false},
		{"deadline", errors.Join(pending, context.DeadlineExceeded), false},
		{"cleanup", &retainedPreparationFixtureError{cause: pending}, false},
		{"unknown", errors.Join(pending, pueue.ErrUnknown), false},
		{"other pending reason", errors.Join(pending, &pueue.PrivateUpgradeError{ReasonCode: "predecessor_wait_pending", Err: pueue.ErrPrivateUpgradePending}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if cleanDrainIncomplete(tc.cause) != tc.want {
				t.Fatalf("drain advancement borrowed another error: %v", tc.cause)
			}
		})
	}
}

// These canonical retained-history inputs prove replay/selection only. They
// never claim actual daemon birth or closure; owner lifetime tests are separate.
func seedCompletedUpgradeAndCurrentDrain(t *testing.T, store *taskdir.Store, binding task.SupervisorRef) (taskdir.RootPin, infrastructure.UpgradeOperationID, infrastructure.UpgradeOperationID) {
	t.Helper()
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	a := seedUpgradeWaitPendingJournal(t, pin, binding, recordedUpgradeWaitPredecessor(t, binding))
	base := filepath.Dir(binding.ConfigPath)
	data, err := os.ReadFile(filepath.Join(base, "migration.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record["legacy_stopped"] = true
	generation, ok := record["upgrade_generation"].(map[string]any)
	if !ok {
		t.Fatal("missing fixture generation")
	}
	generation["accepted_start"], generation["completed"] = true, true
	generation["successor_boot_id"] = generation["legacy_boot_id"]
	generation["successor_pid"], generation["successor_process_start"] = self.PID(), self.StartToken()
	data, err = task.MarshalCanonical(map[string]any{"schema_version": task.SchemaVersion, "migrations": []any{record}})
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(base, "migration-history.json"), data)
	if err = os.Remove(filepath.Join(base, "migration.json")); err != nil {
		t.Fatal(err)
	}
	b := infrastructure.UpgradeOperationID("upgrade-v1:" + strings.Repeat("8", 32))
	data, err = task.MarshalCanonical(map[string]any{"schema_version": task.SchemaVersion, "drain_v1": map[string]any{"root": map[string]any{"path": pin.Root(), "id": pin.RootID(), "device": pin.Device(), "inode": pin.Inode()}, "predecessor": binding, "operation_id": b, "mode": "operation_required", "phase": "draining"}})
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(base, "bootstrap.pending"), data)
	observed, found, err := pueue.ObservePrivateUpgrade(t.Context(), pin, a)
	if err != nil || !found || !observed.Completed {
		t.Fatalf("invalid retained fixture A: %+v %t %v", observed, found, err)
	}
	return pin, infrastructure.UpgradeOperationID(a), b
}

func TestCompletedExactUpgradeReplaysBeforeCurrentDrainAndRefresh(t *testing.T) {
	store, original, owner := newOwnedPreparationProbe(t)
	pin, a, b := seedCompletedUpgradeAndCurrentDrain(t, store, original.Binding())
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	before := supervisorfixture.CapturePairFaultWitness(t, filepath.Dir(original.Binding().ConfigPath))
	preparation := infrastructurePreparationOwner{store: store, options: pueue.Options{PrivateGenerationGuard: owner, DaemonStartGuard: owner, DaemonWaitOwner: owner}}
	err = pueue.WithExistingBootstrapLockContext(ctx, store.Root, func(locked context.Context) error {
		observed, transitionErr := preparation.upgradeLocked(locked, pin, infrastructure.UpgradeRequest{Action: infrastructure.UpgradeComplete, OperationID: a})
		if transitionErr != nil || !observed.Completed || observed.OperationID != pueue.UpgradeOperationID(a) {
			t.Errorf("retained completion advanced current drain: %+v %v", observed, transitionErr)
		}
		return transitionErr
	})
	if err != nil {
		t.Fatal(err)
	}
	// A stale selected owner must never even be contacted after completed A.
	stale := infrastructure.StartupReference{Sequence: 19, Epoch: strings.Repeat("c", 32), IntentSHA256: strings.Repeat("d", 64)}
	contact := infrastructurePreparationContact{resources: &infrastructureRoutingResources{reader: reader, target: store}, selection: infrastructureRoutingSelection{owner: stale}}
	outcome, err := contact.requestUpgrade(ctx, pin, infrastructure.UpgradeRequest{Action: infrastructure.UpgradeComplete, OperationID: a}, true)
	if err != nil || outcome.Condition != infrastructure.UpgradeOK || !outcome.Report.Completed || outcome.Report.OperationID != a {
		t.Fatalf("completed A attempted owner refresh: %+v err=%v", outcome, err)
	}
	current, found, err := pueue.ObservePrivateUpgrade(ctx, pin, pueue.UpgradeOperationID(b))
	if err != nil || !found || current.Phase != pueue.UpgradeDraining {
		t.Fatalf("B changed: %+v %t %v", current, found, err)
	}
	supervisorfixture.AssertPairFaultWitness(t, filepath.Dir(original.Binding().ConfigPath), before)
}
