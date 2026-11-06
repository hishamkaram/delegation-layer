package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The bootstrap record owns the exact A -> B restart. Migration and identity
// publication may read that intent, but cannot replace its target with C.
func TestRestartAuthorityRefusesStoppedMigrationBeforeSuccessorStart(t *testing.T) {
	fixture, predecessor, target := newPrivateReconciliationFixture(t)
	clientPath, daemonPath := installPrivateSupervisorPair(t, filepath.Join(canonicalTemp(t), "third-installation"))
	options := privateMigrationTestOptions(fixture.stateRoot, privateTestObservationTimeout)
	options.DaemonWaitOwner, _ = ownPrivateRecoveryDaemonWaits(t, fixture.stateRoot)
	third := preparePrivateCandidateForTest(t, fixture.stateRoot, clientPath, daemonPath, options)
	// Cleanup is registered before migration can reach its real native Start.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), privateTestObservationTimeout)
		defer cancel()
		if err := third.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := third.WaitUnavailable(ctx); err != nil {
			t.Error(err)
		}
	})
	predecessorPID, present, err := third.privateDaemonPID()
	if err != nil || !present || predecessorPID <= 0 {
		t.Fatalf("read actual predecessor PID: pid=%d present=%t err=%v", predecessorPID, present, err)
	}
	if err = third.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = third.WaitUnavailable(t.Context()); err != nil {
		t.Fatal(err)
	}
	writePrivateSuccessorTestRecord(t, privateBootstrapRecordPath(fixture.stateRoot), privateBootstrapRecord{
		SchemaVersion: task.SchemaVersion, Restart: &privatePairRestartIntent{Predecessor: predecessor, Target: target},
	})
	if err = publishPrivateMigrationState(filepath.Dir(predecessor.ConfigPath), privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(predecessor), Successor: privateDaemonIdentityForBinding(third.Binding()),
		LegacyPID: predecessorPID, LegacyStopped: true,
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	err = WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		return third.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Errorf("stopped migration displaced exact restart target: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	if after := snapshotPrivateSuccessorTestState(t, fixture.stateRoot); !reflect.DeepEqual(before, after) {
		t.Error("conflicting stopped migration changed records, publications, or daemon logs")
	}
}

func TestRestartAuthorityRefusesConflictingIdentityPublicationWithoutMutation(t *testing.T) {
	fixture, predecessor, target := newPrivateReconciliationFixture(t)
	third := target
	third.ClientExecutable, third.DaemonExecutable = installPrivateSupervisorPair(t, filepath.Join(canonicalTemp(t), "third-installation"))
	writePrivateSuccessorTestRecord(t, privateBootstrapRecordPath(fixture.stateRoot), privateBootstrapRecord{
		SchemaVersion: task.SchemaVersion, Restart: &privatePairRestartIntent{Predecessor: predecessor, Target: target},
	})
	writePrivateSuccessorTestRecord(t, privateIdentityPublicationPath(fixture.stateRoot), privateIdentityPublicationRecord{
		SchemaVersion: task.SchemaVersion, Binding: third,
	})
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(context.Context) error {
		replayed, replayErr := recoverPrivateIdentityPublication(fixture.stateRoot)
		if replayed {
			t.Error("conflicting C publication replayed despite exact B restart authority")
		}
		return replayErr
	})
	if !errors.Is(err, ErrBinding) {
		t.Errorf("conflicting identity publication lost identity-conflict cause: %v", err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	if after := snapshotPrivateSuccessorTestState(t, fixture.stateRoot); !reflect.DeepEqual(before, after) {
		t.Error("refused publication changed binding, daemon identity, pending publication, bootstrap, or logs")
	}
}

func TestRestartAuthorityReplaysMatchingReadyIdentityPublicationWithoutStart(t *testing.T) {
	fixture, predecessor, target := newPrivateReconciliationFixture(t)
	options := privateMigrationTestOptions(fixture.stateRoot, privateTestObservationTimeout)
	options.DaemonWaitOwner, _ = ownPrivateRecoveryDaemonWaits(t, fixture.stateRoot)
	waitOwner := options.DaemonWaitOwner
	startedPID := 0
	options.DaemonWaitOwner = daemonWaitOwnerFunc(func(root string, pid int, done <-chan struct{}) error {
		startedPID = pid
		return waitOwner.RegisterDaemonWait(root, pid, done)
	})
	client := preparePrivateCandidateForTest(t, fixture.stateRoot, target.ClientExecutable, target.DaemonExecutable, options)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), privateTestObservationTimeout)
		defer cancel()
		if err := client.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := client.WaitUnavailable(ctx); err != nil {
			t.Error(err)
		}
	})
	if err := client.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := client.WaitUnavailable(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPrivateRestartAbsentForTest(t, fixture.stateRoot)
	// Construct the old binary's recovery-entry process and record state.
	// Current code forbids the old writer-before-Start order. No recovery
	// consumer runs between this actual Start and seeding historical Restart.
	// B's sole Wait is registered with the existing fixture owner.
	if err := startDaemon(t.Context(), fixture.stateRoot, target.DaemonExecutable, target.ConfigPath, filepath.Dir(target.ConfigPath), options); err != nil {
		t.Fatal(err)
	}
	waitPrivateTransitionReadyForTest(t, fixture.stateRoot, client)
	if pid, found, pidErr := client.privateDaemonPID(); pidErr != nil || !found || pid != startedPID || startedPID <= 0 {
		t.Fatalf("ready B PID differs from actual Start: pid=%d started=%d found=%t err=%v", pid, startedPID, found, pidErr)
	}
	writePrivateSuccessorTestRecord(t, privateBootstrapRecordPath(fixture.stateRoot), privateBootstrapRecord{
		SchemaVersion: task.SchemaVersion, Restart: &privatePairRestartIntent{Predecessor: predecessor, Target: target},
	})
	writePrivateSuccessorTestRecord(t, privateIdentityPublicationPath(fixture.stateRoot), privateIdentityPublicationRecord{
		SchemaVersion: task.SchemaVersion, Binding: target,
	})
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(context.Context) error {
		replayed, replayErr := recoverPrivateIdentityPublication(fixture.stateRoot)
		if !replayed && replayErr == nil {
			t.Error("matching B publication was not replayed")
		}
		return replayErr
	})
	if err != nil {
		t.Fatal(err)
	}
	err = WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		_, resumeErr := resumePrivatePairRestart(ctx, fixture.stateRoot, target, privatePairRestartIntent{Predecessor: predecessor, Target: target}, options)
		return resumeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateRestartTargetSettled(t, fixture.stateRoot, target)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}

func assertPrivateRestartTargetSettled(t *testing.T, stateRoot string, target task.SupervisorRef) {
	t.Helper()
	published, found, err := ReadPrivateSupervisorBinding(stateRoot)
	if err != nil || !found || published != target {
		t.Fatalf("matching target not published: found=%t binding=%+v err=%v", found, published, err)
	}
	if pending, pendingErr := PrivateBootstrapPending(stateRoot); pendingErr != nil || pending {
		t.Errorf("matching ready restart did not settle: pending=%t err=%v", pending, pendingErr)
	}
	if _, publishing, publicationErr := readPrivateIdentityPublication(stateRoot); publicationErr != nil || publishing {
		t.Errorf("matching publication did not settle: found=%t err=%v", publishing, publicationErr)
	}
}

func waitPrivateTransitionReadyForTest(t *testing.T, stateRoot string, client *Client) {
	t.Helper()
	readyContext, cancelReady := context.WithTimeout(t.Context(), privateTestObservationTimeout)
	defer cancelReady()
	err := WithBootstrapLockContext(readyContext, stateRoot, func(ctx context.Context) error {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			observation, observeErr := client.availableForTransition(ctx, false)
			if observeErr != nil || observation.ready() {
				return observeErr
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRestartAuthorityDoesNotReplayLivePrepublicationStart(t *testing.T) {
	fixture, predecessor, target := newPrivateReconciliationFixture(t)
	childOwner := newDaemonWaitFixture(t)
	target.DaemonExecutable = childOwner.executable
	options := privateMigrationTestOptions(fixture.stateRoot, privateTestObservationTimeout)
	helperOptions := childOwner.options(nil)
	options.Environment, options.DaemonWaitOwner = helperOptions.Environment, helperOptions.DaemonWaitOwner
	client := preparePrivateCandidateForTest(t, fixture.stateRoot, target.ClientExecutable, target.DaemonExecutable, options)
	if err := client.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := client.WaitUnavailable(t.Context()); err != nil {
		t.Fatal(err)
	}
	intent := privatePairRestartIntent{Predecessor: predecessor, Target: target}
	assertPrivateRestartAbsentForTest(t, fixture.stateRoot)
	postStart := errors.New("caller lost after actual Start before publication")
	firstOptions := options
	firstOptions.DaemonStartGuard = daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
		if err := start(); err != nil {
			return err
		}
		return postStart
	})
	// Construct the old binary's recovery-entry process and record state.
	// Current code forbids the old writer-before-Start order. No recovery
	// consumer runs between this actual Start and seeding historical Restart.
	if err := startDaemon(t.Context(), fixture.stateRoot, target.DaemonExecutable, target.ConfigPath, filepath.Dir(target.ConfigPath), firstOptions); !errors.Is(err, postStart) {
		t.Fatalf("actual prepublication Start fault missing: %v", err)
	}
	childOwner.ready(t) // Producer handshake: real child alive, before PID/socket publication.
	if childOwner.registrations() != 1 {
		t.Fatalf("first actual Start has %d Wait registrations", childOwner.registrations())
	}
	reference := observePrivatePrepublicationChild(t, client, childOwner.pid(), target.Endpoint)
	writePrivateSuccessorTestRecord(t, privateBootstrapRecordPath(fixture.stateRoot), privateBootstrapRecord{
		SchemaVersion: task.SchemaVersion, Restart: &intent,
	})
	before := readPrivateSupervisorFile(t, privateBootstrapRecordPath(fixture.stateRoot))
	assertPrivateRestartDaemonStartRefused(t, fixture.stateRoot, target, options, childOwner)
	assertPrivateTestFileUnchanged(t, privateBootstrapRecordPath(fixture.stateRoot), before, true)
	// This is a hang watchdog, not absence evidence. Native identity checks on
	// both sides prove the original child survives the entire retry.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := WithBootstrapLockContext(ctx, fixture.stateRoot, func(ctx context.Context) error {
		outcome, resumeErr := resumePrivatePairRestart(ctx, fixture.stateRoot, predecessor, intent, options)
		return errors.Join(resumeErr, outcome.ProjectedError())
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) || !errors.Is(err, ErrBinding) || privateRecoveryReason(err) != "transition_pending" || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("retry did not refuse through restart authority before watchdog expiry: %v", err)
	}
	if present, err := lifetime.ProcessPresent(reference); err != nil || !present {
		t.Fatalf("original prepublication child did not survive retry: present=%t err=%v", present, err)
	}
	if got := childOwner.registrations(); got != 1 {
		t.Errorf("retry issued another actual B Start: registrations=%d want=1", got)
	}
	assertPrivateTestFileUnchanged(t, privateBootstrapRecordPath(fixture.stateRoot), before, true)
}

func assertPrivateRestartAbsentForTest(t *testing.T, stateRoot string) {
	t.Helper()
	if pending, err := PrivatePairRestartPending(stateRoot); err != nil || pending {
		t.Fatalf("historical Restart exists before fixture Start: pending=%t err=%v", pending, err)
	}
}

func assertPrivateRestartDaemonStartRefused(t *testing.T, stateRoot string, target task.SupervisorRef, options Options, childOwner *daemonWaitFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := startDaemon(ctx, stateRoot, target.DaemonExecutable, target.ConfigPath, filepath.Dir(target.ConfigPath), options)
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) || !errors.Is(err, ErrBinding) || privateRecoveryReason(err) != "transition_pending" || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("direct Start did not refuse through restart authority before watchdog expiry: %v", err)
	}
	if got := childOwner.registrations(); got != 1 {
		t.Errorf("direct Start issued another actual B Start: registrations=%d want=1", got)
	}
}

func observePrivatePrepublicationChild(t *testing.T, client *Client, childPID int, endpoint string) lifetime.ProcessRef {
	t.Helper()
	reference, err := lifetime.ObserveProcess(childPID)
	if err != nil {
		t.Fatal(err)
	}
	if present, presentErr := lifetime.ProcessPresent(reference); presentErr != nil || !present {
		t.Fatalf("prepublication child not live before retry: present=%t err=%v", present, presentErr)
	}
	if pid, found, pidErr := client.privateDaemonPID(); pidErr != nil || found {
		t.Fatalf("prepublication fixture published PID: pid=%d found=%t err=%v", pid, found, pidErr)
	}
	socketPath, err := privateEndpointSocketPath(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Lstat(socketPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("prepublication fixture has socket or unreadable absence: %v", statErr)
	}
	return reference
}
