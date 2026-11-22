package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
)

// The real journal and production Complete path run actual finite bound control
// commands through their external Start/Wait owner. The predecessor process and
// its still-open owned Wait are modeled data, not native daemon closure proof.
func TestUpgradeAcknowledgedShutdownDoesNotResendBeforePredecessorClosure(t *testing.T) {
	f, commands := newShutdownAcknowledgmentFixture(t)
	authorized, err := f.advance(t)
	if err != nil || authorized.Phase != UpgradeAuthorized {
		t.Fatalf("empty predecessor was not authorized: phase=%s err=%v", authorized.Phase, err)
	}
	first, firstErr := completeNativeUpgrade(t, f.pin, f.accepted.OperationID, f.options, func(context.Context) (string, string, error) {
		return f.currentClient, f.currentDaemon, nil
	})
	assertShutdownAcknowledgmentPending(t, f, first, firstErr)
	if commands.entries.Load() != 1 || commands.starts.Load() != 1 || commands.completed.Load() != 1 || commands.exit.Load() != 0 {
		t.Fatalf("first shutdown lacks one clean actual command completion: entries=%d starts=%d completed=%d exit=%d",
			commands.entries.Load(), commands.starts.Load(), commands.completed.Load(), commands.exit.Load())
	}
	before := loadUnclosedAcknowledgedShutdown(t, f)
	authority := upgradeRetryTransportAuthority(t, f)
	// Protocol counterexample: a successful shutdown response can precede daemon
	// closure while its exact endpoint still answers ready. No process is started.
	writePrivateSuccessorTestBytes(t, filepath.Join(f.stored.base, "ready"), nil)
	second, secondErr := completeNativeUpgrade(t, f.pin, f.accepted.OperationID, f.options, func(context.Context) (string, string, error) {
		t.Error("retained migration reselected its target")
		return "", "", ErrConfiguration
	})
	assertShutdownAcknowledgmentPending(t, f, second, secondErr)
	if commands.entries.Load() != 1 || commands.starts.Load() != 1 || commands.completed.Load() != 1 {
		t.Errorf("acknowledged shutdown was resent before predecessor closure: entries=%d starts=%d completed=%d; want one each",
			commands.entries.Load(), commands.starts.Load(), commands.completed.Load())
	}
	assertUpgradeRetryTransportFences(t, f, before, authority)
	f.options.DaemonWaitOwner = upgradeSettlementFixture{err: errors.Join(ErrPrivateUpgradePending, os.ErrPermission)}
	_, waitErr := completeNativeUpgrade(t, f.pin, f.accepted.OperationID, f.options, func(context.Context) (string, string, error) {
		t.Error("acknowledged migration reselected its target after a Wait fault")
		return "", "", ErrConfiguration
	})
	if !errors.Is(waitErr, os.ErrPermission) || commands.starts.Load() != 1 {
		t.Fatalf("acknowledgement hid a mixed Wait fault or resent shutdown: starts=%d err=%v", commands.starts.Load(), waitErr)
	}
	assertUpgradeRetryTransportFences(t, f, before, authority)
}

type shutdownAcknowledgmentCommands struct {
	entries   atomic.Int32
	starts    atomic.Int32
	completed atomic.Int32
	exit      atomic.Int32
}

func newShutdownAcknowledgmentFixture(t *testing.T) (*upgradeAliasReplayFixture, *shutdownAcknowledgmentCommands) {
	t.Helper()
	f := newUpgradeAliasReplayFixture(t, false)
	f.options.ControlOwnerExecutable = os.Args[0]
	f.options.ObservationTimeout = DefaultObservationTimeout
	f.options.PrivateTerminalEventRunner = privateInstalledCallbackForTest(t, f.stored.root)
	f.options.DaemonWaitOwner = upgradeSettlementFixture{owned: true}
	f.options.processObserver = func(pid int) (privateProcessObservation, error) {
		if pid != 123 {
			return privateProcessObservation{}, ErrBinding
		}
		return privateProcessObservation{Present: true, StartToken: "fixture-acknowledged-predecessor"}, nil
	}
	writeUpgradeAliasReplayRows(t, f.stored.base, 0)
	if err := os.MkdirAll(filepath.Join(f.stored.base, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateSuccessorTestBytes(t, filepath.Join(f.stored.base, "run", "pueue.pid"), []byte("123\n"))
	commands := &shutdownAcknowledgmentCommands{}
	f.options.Observer = func(event CommandEvent) {
		f.observeCommand(event)
		if !slices.Contains(event.Argv, "shutdown") {
			return
		}
		switch event.Stage {
		case "entry":
			commands.entries.Add(1)
		case "started":
			commands.starts.Add(1)
		case "completed":
			commands.exit.Store(int32(event.ExitCode))
			commands.completed.Add(1)
		}
	}
	return f, commands
}

func assertShutdownAcknowledgmentPending(t *testing.T, f *upgradeAliasReplayFixture, observed PrivateUpgradeObservation, cause error) {
	t.Helper()
	var pending *PrivateUpgradeError
	if !errors.As(cause, &pending) || pending.ReasonCode != "predecessor_wait_pending" || !errors.Is(cause, ErrPrivateUpgradePending) ||
		observed.OperationID != f.accepted.OperationID || observed.Phase != UpgradeMigrating || observed.Completed {
		t.Fatalf("owned open predecessor Wait did not remain exact-operation pending: operation=%s phase=%s completed=%t err=%v",
			observed.OperationID, observed.Phase, observed.Completed, cause)
	}
}

func loadUnclosedAcknowledgedShutdown(t *testing.T, f *upgradeAliasReplayFixture) privateMigrationRecord {
	t.Helper()
	record, found, err := loadPrivateMigrationRecord(f.stored.base)
	if err != nil || !found || !record.LegacyShutdownRequested || !record.LegacyShutdownDispatched || !record.LegacyShutdownAcknowledged || record.LegacyStopped ||
		record.UpgradeGeneration.OperationID != f.accepted.OperationID || record.UpgradeGeneration.AcceptedStart {
		t.Fatalf("clean shutdown acknowledgement invented closure or lost its retained migration: found=%t record=%+v err=%v", found, record, err)
	}
	return record
}

func TestOrdinaryMigrationShutdownAcknowledgmentIsMonotonicAndRetainedInHistory(t *testing.T) {
	f := newGenerationGuardFixture(t)
	initial, acknowledged, shutdowns := acknowledgeOrdinaryShutdown(t, f)
	for _, mutate := range []func(*privateMigrationRecord){
		func(record *privateMigrationRecord) { record.LegacyShutdownAcknowledged = false },
		func(record *privateMigrationRecord) { record.LegacyPID++ },
		func(record *privateMigrationRecord) { record.LegacyProcessStart = "another-predecessor" },
	} {
		replacement := acknowledged
		mutate(&replacement)
		if err := replacePrivateMigrationRecord(f.base, acknowledged, replacement); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
			t.Fatalf("acknowledged shutdown changed its durable fact or predecessor birth: %v", err)
		}
	}
	if err := replacePrivateMigrationRecord(f.base, initial, initial); !errors.Is(err, ErrBinding) {
		t.Fatalf("stale pre-acknowledgement writer replaced the canonical record: %v", err)
	}
	stopped := observeOrdinaryAcknowledgedClosure(t, f, acknowledged, shutdowns)
	if err := appendPrivateMigrationHistory(f.base, stopped); err != nil {
		t.Fatal(err)
	}
	history, found, err := loadPrivateMigrationHistory(f.base)
	if err != nil || !found || len(history.Migrations) != 1 || history.Migrations[0] != stopped {
		t.Fatalf("completed history lost acknowledged shutdown: found=%t history=%+v err=%v", found, history, err)
	}
}

func acknowledgeOrdinaryShutdown(t *testing.T, f generationGuardFixture) (privateMigrationRecord, privateMigrationRecord, *atomic.Int32) {
	t.Helper()
	initial := f.record
	initial.LegacyPID, initial.LegacyProcessStart = 123, "ordinary-predecessor"
	initial.LegacyShutdownRequested = true
	if err := publishPrivateMigrationState(f.base, initial); err != nil {
		t.Fatal(err)
	}
	shutdowns := &atomic.Int32{}
	f.client.options.Observer = func(event CommandEvent) {
		if event.Stage == "started" && slices.Contains(event.Argv, "shutdown") {
			shutdowns.Add(1)
		}
	}
	var acknowledged privateMigrationRecord
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		var dispatched bool
		var shutdownErr error
		acknowledged, dispatched, shutdownErr = requestPrivateMigrationShutdown(ctx, f.base, f.client, initial)
		if shutdownErr == nil && !dispatched {
			t.Error("ordinary shutdown was not dispatched")
		}
		return shutdownErr
	})
	if err != nil || !acknowledged.LegacyShutdownAcknowledged || acknowledged.LegacyStopped || shutdowns.Load() != 1 {
		t.Fatalf("ordinary actual command did not record acknowledgement separately from closure: shutdowns=%d record=%+v err=%v", shutdowns.Load(), acknowledged, err)
	}
	return initial, acknowledged, shutdowns
}

func observeOrdinaryAcknowledgedClosure(t *testing.T, f generationGuardFixture, acknowledged privateMigrationRecord, shutdowns *atomic.Int32) privateMigrationRecord {
	t.Helper()
	writePrivateSuccessorTestBytes(t, filepath.Join(f.base, "ready"), nil)
	var observations atomic.Int32
	options := f.client.options
	// These are deterministic model observations: still present at entry, then
	// absent at the closure owner's next check. No native process is fabricated.
	options.processObserver = func(int) (privateProcessObservation, error) {
		if observations.Add(1) == 1 {
			return privateProcessObservation{Present: true, StartToken: acknowledged.LegacyProcessStart}, nil
		}
		return privateProcessObservation{}, nil
	}
	var stopped privateMigrationRecord
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		var observeErr error
		stopped, observeErr = observeDispatchedPrivateMigrationShutdown(ctx, f.base, f.client.Binding(), acknowledged, options)
		return observeErr
	})
	if err != nil || !stopped.LegacyStopped || !stopped.LegacyShutdownAcknowledged || observations.Load() != 2 || shutdowns.Load() != 1 {
		t.Fatalf("ordinary acknowledged retry resent control or skipped absence observation: observations=%d shutdowns=%d record=%+v err=%v", observations.Load(), shutdowns.Load(), stopped, err)
	}
	return stopped
}

func TestUpgradeLegacyDispatchedShutdownWithoutAcknowledgmentCanResend(t *testing.T) {
	f, pending := upgradeUnclosedMigrationFixture(t, true)
	options := f.client.options
	options.ControlOwnerExecutable = os.Args[0]
	options.ObservationTimeout = DefaultObservationTimeout
	options.DaemonWaitOwner = upgradeSettlementFixture{owned: true}
	options.processObserver = func(int) (privateProcessObservation, error) {
		return privateProcessObservation{Present: true, StartToken: pending.LegacyProcessStart}, nil
	}
	var starts atomic.Int32
	options.Observer = func(event CommandEvent) {
		if event.Stage == "started" && slices.Contains(event.Argv, "shutdown") {
			starts.Add(1)
		}
	}
	writePrivateSuccessorTestBytes(t, filepath.Join(f.base, "ready"), nil)
	var after privateMigrationRecord
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		var observeErr error
		after, observeErr = observeDispatchedPrivateMigrationShutdown(ctx, f.base, f.client.Binding(), pending, options)
		return observeErr
	})
	var progress *PrivateUpgradeError
	if !errors.As(err, &progress) || progress.ReasonCode != "predecessor_wait_pending" || starts.Load() != 1 ||
		!after.LegacyShutdownAcknowledged || after.LegacyStopped || after.UpgradeGeneration.AcceptedStart {
		t.Fatalf("legacy dispatched record did not conservatively recover one clean request without closure: starts=%d record=%+v err=%v", starts.Load(), after, err)
	}
}

func TestOrdinaryFailedShutdownDoesNotGainAcknowledgment(t *testing.T) {
	f := newGenerationGuardFixture(t)
	requested := f.record
	requested.LegacyPID, requested.LegacyProcessStart = 123, "refused-predecessor"
	requested.LegacyShutdownRequested = true
	if err := publishPrivateMigrationState(f.base, requested); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, f.client.Binding().ClientExecutable, "#!/bin/sh\nexit 7\n")
	var starts atomic.Int32
	f.client.options.Observer = func(event CommandEvent) {
		if event.Stage == "started" && slices.Contains(event.Argv, "shutdown") {
			starts.Add(1)
		}
	}
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		_, _, shutdownErr := requestPrivateMigrationShutdown(ctx, f.base, f.client, requested)
		return shutdownErr
	})
	after, found, readErr := loadPrivateMigrationRecord(f.base)
	if err == nil || !errors.Is(err, ErrBinding) || starts.Load() != 1 || readErr != nil || !found || after != requested {
		t.Fatalf("failed actual shutdown gained acknowledgement or lost its failure: starts=%d found=%t record=%+v err=%v readErr=%v", starts.Load(), found, after, err, readErr)
	}
}

func TestUpgradeShutdownAcknowledgmentPublicationFaultRemainsHard(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "before_commit"
		if committed {
			name = "after_commit"
		}
		t.Run(name, func(t *testing.T) { testShutdownAcknowledgmentPublicationFault(t, committed) })
	}
}

func testShutdownAcknowledgmentPublicationFault(t *testing.T, committed bool) {
	t.Helper()
	f, commands := newShutdownAcknowledgmentFixture(t)
	if _, err := f.advance(t); err != nil {
		t.Fatal(err)
	}
	original := f.options.PrivateGenerationGuard
	fault := errors.New("acknowledgement publication fault")
	f.options.PrivateGenerationGuard = generationGuardFunc(func(ctx context.Context, root string, commit func() error) error {
		if commands.completed.Load() == 1 {
			current, found, err := loadPrivateMigrationRecord(f.stored.base)
			if err != nil {
				return err
			}
			if found && current.LegacyShutdownRequested && !current.LegacyShutdownDispatched {
				if committed {
					return errors.Join(original.GuardPrivateGenerationCommit(ctx, root, commit), fault)
				}
				return fault
			}
		}
		return original.GuardPrivateGenerationCommit(ctx, root, commit)
	})
	_, err := completeNativeUpgrade(t, f.pin, f.accepted.OperationID, f.options, func(context.Context) (string, string, error) {
		return f.currentClient, f.currentDaemon, nil
	})
	assertShutdownAcknowledgmentPublicationWinner(t, f, commands, committed, fault, err)
	if committed {
		f.options.PrivateGenerationGuard = original
		writePrivateSuccessorTestBytes(t, filepath.Join(f.stored.base, "ready"), nil)
		observed, retryErr := completeNativeUpgrade(t, f.pin, f.accepted.OperationID, f.options, func(context.Context) (string, string, error) {
			t.Error("acknowledgement winner reselected target")
			return "", "", ErrConfiguration
		})
		assertShutdownAcknowledgmentPending(t, f, observed, retryErr)
		if commands.starts.Load() != 1 {
			t.Error("committed acknowledgement was resent after its publication fault")
		}
	}
}

func assertShutdownAcknowledgmentPublicationWinner(t *testing.T, f *upgradeAliasReplayFixture, commands *shutdownAcknowledgmentCommands, committed bool, fault, cause error) {
	t.Helper()
	after, found, readErr := loadPrivateMigrationRecord(f.stored.base)
	if !errors.Is(cause, fault) || readErr != nil || !found || after.LegacyShutdownAcknowledged != committed || after.LegacyShutdownDispatched != committed ||
		after.LegacyStopped || after.UpgradeGeneration.AcceptedStart || commands.starts.Load() != 1 || commands.exit.Load() != 0 {
		t.Fatalf("publication fault lost hard cause, canonical winner or closure fence: committed=%t starts=%d found=%t record=%+v err=%v readErr=%v", committed, commands.starts.Load(), found, after, cause, readErr)
	}
}
