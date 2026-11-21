package control

import (
	"context"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This fixture owns all daemon Starts in its scratch root. Its closed Wait
// inventory is test authority, not a substitute for resident history in production.
type coldUpgradeWaitOwner struct{ upgradeNativeWaitOwner }

func (owner *coldUpgradeWaitOwner) RequireRootDaemonWaitQuiescence(ctx context.Context, _ string) error {
	for _, started := range owner.starts {
		select {
		case <-started.done:
		case <-ctx.Done():
			return ctx.Err()
		default:
			return ErrUnknown
		}
	}
	return ctx.Err()
}

func coldNativeUpgradeFixture(t *testing.T) (nativePrivateSupervisorFixture, *Client, *coldUpgradeWaitOwner, taskdir.RootPin) {
	t.Helper()
	f := newNativePrivateSupervisorFixture(t)
	owner := &coldUpgradeWaitOwner{}
	client, err := f.bindWithDaemonWaitOwner(t, func(legacy DaemonWaitOwner) DaemonWaitOwner {
		owner.legacy = legacy
		return owner
	})
	if err != nil {
		t.Fatal(err)
	}
	f.stopRecordedDaemon(t, client)
	if len(owner.starts) != 1 || !joinDaemonWait(t, owner.starts[0].done) {
		t.Fatal("original predecessor Wait did not close")
	}
	pin, err := readControlRootPin(t.Context(), f.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	client.options.PrivateMigrationAuthorizer = func(ctx context.Context, root string, _ task.SupervisorRef, jobs []Job) error {
		if root != f.stateRoot || len(jobs) != 0 {
			return ErrPrivateSupervisorMigrationBusy
		}
		return requireRuntimeRestartBootstrapEX(ctx, root)
	}
	return f, client, owner, pin
}

func TestPrivateSupervisorNativeUpgradeRecoversColdPredecessor(t *testing.T) {
	f, client, owner, pin := coldNativeUpgradeFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var accepted, advanced PrivateUpgradeObservation
	err := WithExistingBootstrapLockContext(ctx, f.stateRoot, func(locked context.Context) error {
		var beginErr error
		accepted, beginErr = BeginPrivateUpgradeWithLock(locked, pin, UpgradeOperationRequired, client.options)
		if beginErr != nil {
			return beginErr
		}
		advanced, beginErr = AdvancePrivateUpgradeDrainWithLock(locked, pin, accepted.OperationID, client.options)
		return beginErr
	})
	if err != nil || advanced.OperationID != accepted.OperationID || advanced.Phase != UpgradeAwaitingInstallation {
		t.Fatalf("cold predecessor did not resume the same retained drain: accepted=%+v advanced=%+v err=%v", accepted, advanced, err)
	}
	if len(owner.starts) != 2 {
		t.Fatalf("cold predecessor recovery Starts=%d, want original plus one recovery", len(owner.starts))
	}
	err = WithExistingBootstrapLockContext(ctx, f.stateRoot, func(locked context.Context) error {
		replayed, replayErr := AdvancePrivateUpgradeDrainWithLock(locked, pin, accepted.OperationID, client.options)
		if replayErr != nil || replayed != advanced || len(owner.starts) != 2 {
			t.Fatalf("advanced drain repeated predecessor Start: starts=%d err=%v", len(owner.starts), replayErr)
		}
		_, authorizeErr := AuthorizePrivateUpgradeCompletionWithLock(locked, pin, accepted.OperationID, client.options)
		return authorizeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNativeColdDrainReady(t, ctx, pin, accepted.OperationID, client.options)
	completed, err := completeNativeUpgradeAfterOwnedWait(t, pin, accepted.OperationID, client.options,
		func(context.Context) (string, string, error) { return f.clientPath, f.daemonPath, nil }, &owner.upgradeNativeWaitOwner)
	if err != nil || !completed.Completed || completed.OperationID != accepted.OperationID || len(owner.starts) != 3 {
		t.Fatalf("same operation did not complete canonical handoff: completed=%+v starts=%d err=%v", completed, len(owner.starts), err)
	}
	assertNativeUpgradeFreshBirth(t, &owner.upgradeNativeWaitOwner, 1, 2)
	// The fixture's original binding owns cleanup for the unchanged generation.
	// It records and joins the recovered daemon's Wait through the same owner.
}

func assertNativeColdDrainReady(t *testing.T, ctx context.Context, pin taskdir.RootPin, op UpgradeOperationID, options Options) {
	t.Helper()
	drain, found, err := readPrivateUpgrade(pin, op)
	if err != nil || !found {
		t.Fatalf("retained drain read: found=%t err=%v", found, err)
	}
	recovered, err := preparePrivateUpgradePredecessor(ctx, pin, drain, options)
	if err != nil {
		t.Fatalf("recovered predecessor is not ready: %v", err)
	}
	if err = recovered.Close(); err != nil {
		t.Fatal(err)
	}
}
