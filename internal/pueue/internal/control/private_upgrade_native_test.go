package control

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This fixture owns every native Start/Wait in its one scratch root. Its
// exhaustive local collection is not a production resident-history proof.
type upgradeNativeWaitOwner struct {
	runtimeRestartTestWaitOwner
	holdPredecessor bool
}

func (owner *upgradeNativeWaitOwner) ObserveDaemonWait(ctx context.Context, _ string, expected lifetime.ProcessRef) (bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	for index, started := range owner.starts {
		birth, ready, err := started.birth()
		if err != nil || !ready {
			return false, false, errors.Join(ErrUnknown, err)
		}
		same, err := lifetime.SameProcess(birth, expected)
		if err != nil {
			return false, false, err
		}
		if !same {
			continue
		}
		if index == 0 && owner.holdPredecessor {
			return true, false, nil
		}
		select {
		case <-started.done:
			return true, true, nil
		default:
			return true, false, nil
		}
	}
	return false, false, nil
}

func upgradeNativeFixture(t *testing.T) (nativePrivateSupervisorFixture, *Client, *upgradeNativeWaitOwner, taskdir.RootPin) {
	t.Helper()
	f := newNativePrivateSupervisorFixture(t)
	owner := &upgradeNativeWaitOwner{}
	client, err := f.bindWithDaemonWaitOwner(t, func(legacy DaemonWaitOwner) DaemonWaitOwner { owner.legacy = legacy; return owner })
	if err != nil {
		t.Fatal(err)
	}
	client.options.PrivateMigrationAuthorizer = func(ctx context.Context, root string, _ task.SupervisorRef, jobs []Job) error {
		if root != f.stateRoot || len(jobs) != 0 {
			return ErrPrivateSupervisorMigrationBusy
		}
		return requireRuntimeRestartBootstrapEX(ctx, root)
	}
	pin, err := readControlRootPin(t.Context(), f.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	return f, client, owner, pin
}

func prepareNativeUpgrade(t *testing.T, pin taskdir.RootPin, options Options) PrivateUpgradeObservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var observed PrivateUpgradeObservation
	for _, step := range []func(context.Context) error{
		func(locked context.Context) error {
			var err error
			observed, err = BeginPrivateUpgradeWithLock(locked, pin, UpgradeOperationRequired, options)
			return err
		},
		func(locked context.Context) error {
			var err error
			observed, err = AdvancePrivateUpgradeDrainWithLock(locked, pin, observed.OperationID, options)
			return err
		},
		func(locked context.Context) error {
			var err error
			observed, err = AuthorizePrivateUpgradeCompletionWithLock(locked, pin, observed.OperationID, options)
			return err
		},
	} {
		if err := WithExistingBootstrapLockContext(ctx, pin.Root(), step); err != nil {
			t.Fatal(err)
		}
	}
	if observed.Phase != UpgradeAuthorized {
		t.Fatalf("native prepare phase = %s", observed.Phase)
	}
	return observed
}

func completeNativeUpgrade(t *testing.T, pin taskdir.RootPin, op UpgradeOperationID, options Options, selector CompatiblePairSelector) (PrivateUpgradeObservation, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var observed PrivateUpgradeObservation
	err := WithExistingBootstrapLockContext(ctx, pin.Root(), func(locked context.Context) error {
		client, value, err := CompletePrivateUpgradeWithLock(locked, pin, op, selector, options)
		observed = value
		if client != nil {
			return errors.Join(err, client.CloseAfterPending(err))
		}
		return err
	})
	return observed, err
}

func TestPrivateSupervisorNativeUpgradeSamePairRefreshIsOperationBound(t *testing.T) {
	f, client, owner, pin := upgradeNativeFixture(t)
	selected := 0
	selector := func(context.Context) (string, string, error) { selected++; return f.clientPath, f.daemonPath, nil }
	first := prepareNativeUpgrade(t, pin, client.options)
	completed, err := completeNativeUpgradeAfterOwnedWait(t, pin, first.OperationID, client.options, selector, owner)
	if err != nil || !completed.Completed {
		t.Fatalf("same-pair completion: %+v err=%v", completed, err)
	}
	assertNativeUpgradeFreshBirth(t, owner, 0, 1)
	for range 2 {
		replayed, replayErr := completeNativeUpgrade(t, pin, first.OperationID, client.options, selector)
		if replayErr != nil || replayed != completed || len(owner.starts) != 2 || selected != 1 {
			t.Fatalf("completed OP replay restarted: %+v starts=%d selections=%d err=%v", replayed, len(owner.starts), selected, replayErr)
		}
	}
	second := prepareNativeUpgrade(t, pin, client.options)
	if second.OperationID == first.OperationID {
		t.Fatal("fresh prepare reused completed OP")
	}
	if _, err = completeNativeUpgradeAfterOwnedWait(t, pin, second.OperationID, client.options, selector, owner); err != nil {
		t.Fatal(err)
	}
	assertNativeUpgradeFreshBirth(t, owner, 1, 2)
	history, found, err := loadPrivateMigrationHistory(filepath.Join(f.stateRoot, privateSupervisorDirectory))
	if err != nil || !found || len(history.Migrations) != 2 {
		t.Fatalf("refresh history: found=%t len=%d err=%v", found, len(history.Migrations), err)
	}
	identity := privateDaemonIdentityForBinding(client.Binding())
	if edge, err := privateSupervisorMigrationEdgeFound(t.Context(), history, identity, identity); err != nil || edge {
		t.Fatalf("same-binding refresh became installation edge: edge=%t err=%v", edge, err)
	}
}

// Shutdown acknowledgement may precede the real predecessor Wait. Observe its
// exact completion outside bootstrap EX, then resume once with the pinned OP.
// Any other pending/fault is returned, never blindly retried.
func completeNativeUpgradeAfterOwnedWait(t *testing.T, pin taskdir.RootPin, op UpgradeOperationID, options Options, selector CompatiblePairSelector, owner *upgradeNativeWaitOwner) (PrivateUpgradeObservation, error) {
	t.Helper()
	startsBefore := len(owner.starts)
	observed, err := completeNativeUpgrade(t, pin, op, options, selector)
	var pending *PrivateUpgradeError
	if !errors.As(err, &pending) || pending.ReasonCode != "predecessor_wait_pending" {
		return observed, err
	}
	if len(owner.starts) != startsBefore {
		t.Fatal("unfinished predecessor Wait admitted successor Start")
	}
	record, found, readErr := loadPrivateMigrationRecord(filepath.Join(pin.Root(), privateSupervisorDirectory))
	if readErr != nil || !found || record.LegacyStopped {
		t.Fatalf("pending actual Wait published predecessor closure: found=%t err=%v", found, readErr)
	}
	if !joinDaemonWait(t, owner.starts[startsBefore-1].done) {
		t.Fatal("actual predecessor Wait did not close outside EX")
	}
	return completeNativeUpgrade(t, pin, op, options, func(context.Context) (string, string, error) {
		t.Fatal("Wait-settled resume reselected pinned pair")
		return "", "", ErrConfiguration
	})
}

func assertNativeUpgradeFreshBirth(t *testing.T, owner *upgradeNativeWaitOwner, before, after int) {
	t.Helper()
	if len(owner.starts) != after+1 {
		t.Fatalf("native Starts=%d want=%d", len(owner.starts), after+1)
	}
	old, err := awaitDaemonBirth(t, owner.starts[before].birth)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := awaitDaemonBirth(t, owner.starts[after].birth)
	if err != nil {
		t.Fatal(err)
	}
	same, err := lifetime.SameProcess(old, fresh)
	if err != nil || same {
		t.Fatalf("refresh did not change native incarnation: same=%t err=%v", same, err)
	}
	if !joinDaemonWait(t, owner.starts[before].done) {
		t.Fatal("predecessor actual Wait did not close")
	}
	if present, err := lifetime.ProcessPresent(old); err != nil || present {
		t.Fatalf("predecessor remains present: present=%t err=%v", present, err)
	}
}

func TestPrivateSupervisorNativeUpgradeWaitPrecedesAbsenceFallback(t *testing.T) {
	f, client, owner, pin := upgradeNativeFixture(t)
	owner.holdPredecessor = true
	prepared := prepareNativeUpgrade(t, pin, client.options)
	selector := func(context.Context) (string, string, error) { return f.clientPath, f.daemonPath, nil }
	if _, err := completeNativeUpgrade(t, pin, prepared.OperationID, client.options, selector); !errors.Is(err, ErrPrivateUpgradePending) {
		t.Fatalf("unfinished Wait did not hold fence: %v", err)
	}
	if len(owner.starts) != 1 {
		t.Fatal("native absence bypassed predecessor Wait")
	}
	pending, found, err := loadPrivateMigrationRecord(filepath.Join(f.stateRoot, privateSupervisorDirectory))
	if err != nil || !found || pending.LegacyStopped {
		t.Fatalf("unfinished Wait published closure: %+v found=%t err=%v", pending, found, err)
	}
	owner.holdPredecessor = false
	if !joinDaemonWait(t, owner.starts[0].done) {
		t.Fatal("predecessor actual Wait did not close outside EX")
	}
	if _, err = completeNativeUpgrade(t, pin, prepared.OperationID, client.options, func(context.Context) (string, string, error) {
		t.Fatal("resume reselected pinned pair")
		return "", "", ErrConfiguration
	}); err != nil {
		t.Fatal(err)
	}
	assertNativeUpgradeFreshBirth(t, owner, 0, 1)
}
