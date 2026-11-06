package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPublishedPairReconcilesPathSplit(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "stopped"}[stopped], func(t *testing.T) {
			testPublishedPairPathSplit(t, stopped)
		})
	}
}

func TestPreparePrivateMigrationPinsPublishedResolver(t *testing.T) {
	fixture, original, _ := newPrivateReconciliationFixture(t)
	t.Chdir(canonicalTemp(t))
	var prepared *Client
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		var prepareErr error
		prepared, prepareErr = PreparePrivateForMigrationWithLock(ctx, fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout})
		return prepareErr
	})
	if prepared != nil {
		t.Cleanup(func() {
			if closeErr := prepared.CloseAfterPending(err); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Binding() != original {
		t.Fatalf("caller cwd replaced published resolver: got=%+v want=%+v", prepared.Binding(), original)
	}
}

func testPublishedPairPathSplit(t *testing.T, stopped bool) {
	t.Helper()
	fixture, original, candidate := newPrivateReconciliationFixture(t)
	identity := privateDaemonIdentityForBinding(candidate)
	identityPath := privateDaemonIdentityPath(filepath.Dir(original.ConfigPath))
	writePrivateSuccessorTestRecord(t, identityPath, identity)
	before := readPrivateSupervisorFile(t, identityPath)
	if stopped {
		if err := os.Remove(filepath.Join(filepath.Dir(original.ConfigPath), "ready")); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := reconcilePrivateTestPair(t, fixture, candidate); err != nil {
			t.Fatal(err)
		}
	}
	got, found, err := ReadPrivateSupervisorBinding(fixture.stateRoot)
	if err != nil || !found || got != candidate {
		t.Fatalf("binding not repaired: %+v found=%t err=%v", got, found, err)
	}
	if after := readPrivateSupervisorFile(t, identityPath); !bytes.Equal(before, after) {
		t.Fatal("metadata reconciliation rewrote daemon provenance")
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestPublishedPairRefusesUnrecordedStoppedReplacement(t *testing.T) {
	fixture, original, candidate := newPrivateReconciliationFixture(t)
	for _, path := range []string{original.ClientExecutable, original.DaemonExecutable, filepath.Join(filepath.Dir(original.ConfigPath), "ready")} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	for range 2 {
		err := reconcilePrivateTestPair(t, fixture, candidate)
		if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) || privateRecoveryReason(err) != "predecessor_pair_unavailable" {
			t.Fatalf("unrecorded stopped replacement = %v, want predecessor_pair_unavailable", err)
		}
		if after := snapshotPrivateSuccessorTestState(t, fixture.stateRoot); !reflect.DeepEqual(before, after) {
			t.Fatal("unrecorded stopped replacement changed private state or daemon logs")
		}
		assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
	}
}

func TestPublishedHistoricalPairConflictRefusesBeforeSelection(t *testing.T) {
	fixture, published, candidate := newPrivateReconciliationFixture(t)
	published.ExecutableIdentity = ""
	published.ClientSHA256, published.DaemonSHA256 = strings.Repeat("a", 64), strings.Repeat("b", 64)
	writePrivateSuccessorTestRecord(t, privateSupervisorBindingPath(fixture.stateRoot), privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: published})
	writePrivateSuccessorTestRecord(t, privateDaemonIdentityPath(filepath.Dir(published.ConfigPath)), privateDaemonIdentityForBinding(candidate))
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	selected := false
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		_, err := ReconcilePublishedPrivatePairAttemptWithLock(ctx, fixture.stateRoot, func(context.Context) (string, string, error) {
			selected = true
			return candidate.ClientExecutable, candidate.DaemonExecutable, nil
		}, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}})
		return err
	})
	if !errors.Is(err, ErrBinding) || !errors.Is(err, ErrPrivateSupervisorMigrationRequired) || privateRecoveryReason(err) != "identity_conflict" || selected {
		t.Fatalf("historical conflict reached replacement selection: selected=%t err=%v", selected, err)
	}
	if after := snapshotPrivateSuccessorTestState(t, fixture.stateRoot); !reflect.DeepEqual(before, after) {
		t.Fatal("historical conflict changed private state or daemon logs")
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestSupervisorUpgradePreservesConflictingState(t *testing.T) {
	for _, fault := range []string{"live-missing-pair", "bootstrap", "history-predecessor", "history-successor"} {
		t.Run(fault, func(t *testing.T) {
			fixture, original, candidate := newPrivateReconciliationFixture(t)
			identity := privateDaemonIdentityForBinding(candidate)
			if fault == "live-missing-pair" {
				identity = privateDaemonIdentityForBinding(original)
				if err := os.Remove(original.ClientExecutable); err != nil {
					t.Fatal(err)
				}
			}
			identityPath := privateDaemonIdentityPath(filepath.Dir(original.ConfigPath))
			writePrivateSuccessorTestRecord(t, identityPath, identity)
			injectPrivateReconciliationFault(t, fixture, original, identity, fault)
			bindingBefore := readPrivateSupervisorFile(t, privateSupervisorBindingPath(fixture.stateRoot))
			identityBefore := readPrivateSupervisorFile(t, identityPath)
			err := reconcilePrivateTestPair(t, fixture, candidate)
			if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
				t.Fatalf("unproven reconciliation accepted: %v", err)
			}
			assertPrivateTestFileUnchanged(t, privateSupervisorBindingPath(fixture.stateRoot), bindingBefore, true)
			assertPrivateTestFileUnchanged(t, identityPath, identityBefore, true)
			assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
		})
	}
}

func injectPrivateReconciliationFault(t *testing.T, fixture privateSupervisorTestFixture, original task.SupervisorRef, identity privateDaemonIdentity, fault string) {
	t.Helper()
	switch fault {
	case "bootstrap":
		if err := markPrivateBootstrapPending(fixture.stateRoot); err != nil {
			t.Fatal(err)
		}
	case "history-predecessor", "history-successor":
		other := identity
		other.Executable += "-other"
		edge := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: identity, Successor: other, LegacyPID: 123, LegacyStopped: true}
		if fault == "history-successor" {
			edge.Legacy, edge.Successor = other, identity
		}
		writePrivateSuccessorTestRecord(t, filepath.Join(filepath.Dir(original.ConfigPath), privateMigrationHistoryRecordName), privateMigrationHistory{SchemaVersion: task.SchemaVersion, Migrations: []privateMigrationRecord{edge}})
	}
}

func TestInspectionPreservesVerifiedPredecessor(t *testing.T) {
	fixture, original, candidate := newPrivateReconciliationFixture(t)
	if err := os.Remove(privateSupervisorBindingPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	if err := PublishVerifiedPrivatePredecessorWithOptionsAndLock(t.Context(), fixture.stateRoot, candidate, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}}); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unverified predecessor accepted: %v", err)
	}
	if err := PublishVerifiedPrivatePredecessorWithOptionsAndLock(t.Context(), fixture.stateRoot, original, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}}); err != nil {
		t.Fatal(err)
	}
	got, found, err := ReadPrivateSupervisorBinding(fixture.stateRoot)
	if err != nil || !found || got != original {
		t.Fatalf("actual predecessor not preserved: %+v found=%t err=%v", got, found, err)
	}
}

func newPrivateReconciliationFixture(t *testing.T) (privateSupervisorTestFixture, task.SupervisorRef, task.SupervisorRef) {
	t.Helper()
	fixture := newPrivateSupervisorTestFixture(t)
	const readyCommand = `touch "$(dirname "$config")/ready"`
	beforeReady, afterReady, found := strings.Cut(privateDaemonFixture, readyCommand)
	if !found {
		t.Fatal("daemon fixture readiness command missing")
	}
	// Install publication faults before the client can observe readiness.
	daemonSource := beforeReady + `
if [ -f "$(dirname "$config")/block-identity-publication" ]; then
  mkdir "$(dirname "$config")/identity.pending.json"
fi
if [ -f "$(dirname "$config")/block-daemon-publication" ]; then
  mv "$(dirname "$config")/daemon.identity.json" "$(dirname "$config")/daemon.identity.before-fault.json"
  mkdir "$(dirname "$config")/daemon.identity.json"
fi
` + readyCommand + afterReady
	writeExecutable(t, fixture.daemonPath, daemonSource)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	original := client.Binding()
	candidate := original
	directory := filepath.Join(filepath.Dir(fixture.stateRoot), "installed-pair")
	makePrivateTestDirectory(t, directory)
	candidate.ClientExecutable, candidate.DaemonExecutable = filepath.Join(directory, "pueue"), filepath.Join(directory, "pueued")
	writeExecutable(t, candidate.ClientExecutable, privateClientFixture)
	writeExecutable(t, candidate.DaemonExecutable, daemonSource)
	return fixture, original, candidate
}

func TestPublishedPairRestartRecoversLaunchBeforePublication(t *testing.T) {
	testPrivateRestartPublicationFault(t, "block-identity-publication", "identity.pending.json")
}

func TestPublishedPairRestartRecoversInterruptedIdentityPublication(t *testing.T) {
	testPrivateRestartPublicationFault(t, "block-daemon-publication", "daemon.identity.json")
}

func testPrivateRestartPublicationFault(t *testing.T, faultMarker, faultDirectory string) {
	t.Helper()
	fixture, candidate := interruptPrivateRestartPublication(t, faultMarker)
	if err := os.Remove(filepath.Join(filepath.Dir(candidate.ConfigPath), faultDirectory)); err != nil {
		t.Fatal(err)
	}
	if err := reconcilePrivateTestPair(t, fixture, candidate); err != nil {
		t.Fatal(err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	complete, err := PrivateSupervisorMigrationComplete(fixture.stateRoot, candidate)
	if err != nil || !complete {
		t.Fatalf("restart did not finish publication: complete=%t err=%v", complete, err)
	}
}

func interruptPrivateRestartPublication(t *testing.T, faultMarker string) (privateSupervisorTestFixture, task.SupervisorRef) {
	t.Helper()
	fixture, original, candidate := newPrivateReconciliationFixture(t)
	base := filepath.Dir(original.ConfigPath)
	options := privateMigrationTestOptions(fixture.stateRoot, privateTestObservationTimeout)
	waitOwner := privateFixtureDaemonWaitOwner(t)
	startedPID := 0
	options.DaemonWaitOwner = daemonWaitOwnerFunc(func(root string, pid int, done <-chan struct{}) error {
		startedPID = pid
		return waitOwner.RegisterDaemonWait(root, pid, done)
	})
	client := preparePrivateCandidateForTest(t, fixture.stateRoot, candidate.ClientExecutable, candidate.DaemonExecutable, options)
	for _, path := range []string{original.ClientExecutable, original.DaemonExecutable, filepath.Join(base, "ready")} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, faultMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the old binary's recovery-entry snapshot: B actually started
	// and became ready before a crash left its exact historical restart intent.
	if err := startDaemon(t.Context(), fixture.stateRoot, candidate.DaemonExecutable, candidate.ConfigPath, base, options); err != nil {
		t.Fatal(err)
	}
	waitPrivateTransitionReadyForTest(t, fixture.stateRoot, client)
	if pid, found, err := client.privateDaemonPID(); err != nil || !found || pid != startedPID || startedPID <= 0 {
		t.Fatalf("ready target PID differs from actual Start: pid=%d started=%d found=%t err=%v", pid, startedPID, found, err)
	}
	publishTestPairRestartIntent(t, fixture.stateRoot, original, candidate)
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		return publishPrivateDaemonIdentityContext(ctx, base, candidate, options.PrivateGenerationGuard)
	})
	if err == nil {
		t.Fatal("publication fault was not exercised")
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	record, found, err := readPrivateBootstrapRecord(fixture.stateRoot)
	if err != nil || !found || record.Restart == nil || record.Restart.Target != candidate {
		t.Fatalf("exact restart intent lost after actual launch: %+v found=%t err=%v", record, found, err)
	}
	return fixture, candidate
}

func reconcilePrivateTestPair(t *testing.T, fixture privateSupervisorTestFixture, candidate task.SupervisorRef) error {
	t.Helper()
	return WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		outcome, err := ReconcilePublishedPrivatePairAttemptWithLock(ctx, fixture.stateRoot, func(context.Context) (string, string, error) {
			return candidate.ClientExecutable, candidate.DaemonExecutable, nil
		}, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: privateTestObservationTimeout})
		if err != nil {
			return err
		}
		return outcome.ProjectedError()
	})
}
