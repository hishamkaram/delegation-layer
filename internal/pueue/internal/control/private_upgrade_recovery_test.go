package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/migration"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Native tokens below are persisted fixture inputs. These tests prove storage
// recovery and Start ownership, never native predecessor closure.
func upgradeMigrationStorageFixture(t *testing.T) (generationGuardFixture, taskdir.RootPin, privateMigrationRecord) {
	t.Helper()
	f, pin, accepted := upgradeJournalFixture(t, UpgradeOperationRequired)
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	target := f.client.Binding()
	target.DaemonExecutable = filepath.Join(filepath.Dir(target.DaemonExecutable), "next-pueued")
	pending := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(f.client.Binding()), Successor: privateDaemonIdentityForBinding(target), LegacyPID: 123, LegacyProcessStart: "old-native-start", LegacyShutdownRequested: true, LegacyShutdownDispatched: true, LegacyStopped: true, UpgradeGeneration: migration.UpgradeGeneration{OperationID: accepted.OperationID, Root: upgradeRootSubject(pin), Predecessor: f.client.Binding(), Mode: UpgradeOperationRequired, Target: target, LegacyBootID: boot}}
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		drain, found, readErr := readPrivateUpgrade(pin, accepted.OperationID)
		if readErr != nil || !found {
			return errors.Join(ErrBinding, readErr)
		}
		authorized := drain
		authorized.Phase = UpgradeAuthorized
		authorized.CompletionAuthorization = privateUpgradeOperationRef{OperationID: accepted.OperationID}
		authorized.SelectedPair = privateUpgradePair{ClientExecutable: target.ClientExecutable, DaemonExecutable: target.DaemonExecutable}
		if writeErr := replacePrivateUpgrade(ctx, pin, drain, authorized, f.client.options); writeErr != nil {
			return writeErr
		}
		if writeErr := publishPrivateMigrationStateContext(ctx, f.base, pending, f.client.options.PrivateGenerationGuard); writeErr != nil {
			return writeErr
		}
		_, linkErr := linkPrivateUpgradeMigration(ctx, pin, authorized, pending, f.client.options)
		return linkErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, pin, pending
}

func TestUpgradePartialIdentityPublicationUsesExactOwnerRecovery(t *testing.T) {
	for _, phase := range []string{"pending", "binding", "daemon", "history", "migration_removed"} {
		t.Run(phase, func(t *testing.T) { testUpgradePublicationRecovery(t, phase) })
	}
}

func testUpgradePublicationRecovery(t *testing.T, phase string) {
	t.Helper()
	f, pin, pending := upgradeMigrationStorageFixture(t)
	completed := pending
	completed.UpgradeGeneration.AcceptedStart = true
	completed.UpgradeGeneration.SuccessorBootID = completed.UpgradeGeneration.LegacyBootID
	completed.UpgradeGeneration.SuccessorPID = 124
	completed.UpgradeGeneration.SuccessorProcessStart = "new-native-start"
	completed.UpgradeGeneration.Completed = true
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		if err := replacePrivateMigrationRecordContext(ctx, f.base, pending, completed, f.client.options.PrivateGenerationGuard); err != nil {
			return err
		}
		if err := stageUpgradePublicationInterruption(f.root, f.base, completed, phase); err != nil {
			return err
		}
		if phase != "history" && phase != "migration_removed" {
			if _, err := recoverPrivateIdentityPublication(f.root); !errors.Is(err, ErrPrivateUpgradePending) {
				t.Fatalf("generic identity recovery crossed drain: %v", err)
			}
		}
		options := f.client.options
		options.DaemonWaitOwner = &upgradeNativeWaitOwner{}
		client, observed, completeErr := CompletePrivateUpgradeWithLock(ctx, pin, completed.UpgradeGeneration.OperationID, func(context.Context) (string, string, error) {
			t.Fatal("completed native generation reselected target")
			return "", "", ErrConfiguration
		}, options)
		if completeErr != nil {
			return completeErr
		}
		if !observed.Completed {
			t.Fatal("verified completion did not finish through public consumer")
		}
		if client != nil {
			return client.Close()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertUpgradePublicationWinner(t, f, pin, completed)
}

func stageUpgradePublicationInterruption(root, base string, record privateMigrationRecord, phase string) error {
	publication := privateIdentityPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: record.UpgradeGeneration.Target}
	data, err := task.MarshalCanonical(publication)
	if err != nil {
		return err
	}
	if err = publishPrivateRecord(base, privateIdentityPublicationPath(root), data); err != nil || phase == "pending" {
		return err
	}
	data, err = task.MarshalCanonical(privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: publication.Binding})
	if err != nil {
		return err
	}
	if err = replacePrivateSupervisorBindingRecord(root, data); err != nil || phase == "binding" {
		return err
	}
	data, err = task.MarshalCanonical(privateDaemonIdentityForBinding(publication.Binding))
	if err != nil {
		return err
	}
	if err = replacePrivateDaemonIdentityRecord(base, data); err != nil || phase == "daemon" {
		return err
	}
	if err = publishPrivateIdentityPublicationValidated(root, publication); err != nil {
		return err
	}
	if err = appendPrivateMigrationHistory(base, record); err != nil || phase == "history" {
		return err
	}
	return clearPrivateMigrationRecord(base, record)
}

func assertUpgradePublicationWinner(t *testing.T, f generationGuardFixture, pin taskdir.RootPin, completed privateMigrationRecord) {
	t.Helper()
	if _, found, err := readPrivateBootstrapRecord(f.root); err != nil || found {
		t.Fatalf("completed publication retained fence: found=%t err=%v", found, err)
	}
	if _, found, err := loadPrivateMigrationRecord(f.base); err != nil || found {
		t.Fatalf("completed publication retained migration: found=%t err=%v", found, err)
	}
	history, found, err := loadPrivateMigrationHistory(f.base)
	if err != nil || !found || len(history.Migrations) != 1 || history.Migrations[0] != completed {
		t.Fatalf("completion history changed: found=%t err=%v", found, err)
	}
	// Target was never installed in this storage fixture: historical replay
	// must not open it, select again, or claim current admission readiness.
	var replay PrivateUpgradeObservation
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		client, observed, replayErr := CompletePrivateUpgradeWithLock(ctx, pin, completed.UpgradeGeneration.OperationID, func(context.Context) (string, string, error) {
			t.Fatal("historical completion reselected missing target")
			return "", "", ErrConfiguration
		}, f.client.options)
		replay = observed
		if client != nil {
			return errors.Join(replayErr, client.Close())
		}
		return replayErr
	})
	if err != nil || !replay.Completed {
		t.Fatalf("historical completion required target presence: %+v err=%v", replay, err)
	}
}

func TestUpgradeDefiniteStartFailureCanRetryWithoutDuplicateStart(t *testing.T) {
	f, pin, pending := upgradeMigrationStorageFixture(t)
	target := pending.UpgradeGeneration.Target
	writeExecutable(t, target.DaemonExecutable, "#!/missing-owned-interpreter\n")
	client, owner := newUpgradeStartFixtureClient(t, target, f.client.options)
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		_, startErr := client.startPrivateUpgradeSuccessor(ctx, pin, f.base, pending)
		if startErr == nil {
			t.Fatal("missing interpreter Start unexpectedly succeeded")
		}
		current, found, readErr := loadPrivateMigrationRecord(f.base)
		if readErr != nil || !found || current.UpgradeGeneration.AcceptedStart || len(owner.starts) != 0 {
			t.Fatalf("definite no-child failure retained acceptance: %+v found=%t err=%v", current, found, readErr)
		}
		writeExecutable(t, target.DaemonExecutable, "#!/bin/sh\nexit 0\n")
		started, startErr := client.startPrivateUpgradeSuccessor(ctx, pin, f.base, current)
		if startErr != nil || !started.UpgradeGeneration.AcceptedStart || started.UpgradeGeneration.SuccessorPID <= 0 || len(owner.starts) != 1 {
			t.Fatalf("corrected Start did not own one birth: %+v starts=%d err=%v", started, len(owner.starts), startErr)
		}
		_, retryErr := client.startPrivateUpgradeSuccessor(ctx, pin, f.base, started)
		if retryErr == nil || len(owner.starts) != 1 {
			t.Fatalf("accepted child replay launched again: starts=%d err=%v", len(owner.starts), retryErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target.DaemonExecutable); err != nil {
		t.Fatal(err)
	}
}

func newUpgradeStartFixtureClient(t *testing.T, target task.SupervisorRef, options Options) (*Client, *upgradeNativeWaitOwner) {
	t.Helper()
	owner := &upgradeNativeWaitOwner{}
	owner.legacy = daemonWaitOwnerFunc(func(string, int, <-chan struct{}) error { return nil })
	options.DaemonStartGuard = allowDaemonStartForTest{}
	options.DaemonWaitOwner = owner
	client, err := NewClient(target, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, started := range owner.starts {
			if !joinDaemonWait(t, started.done) {
				t.Error("owned fixture Start/Wait did not join")
			}
		}
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client, owner
}

func TestUpgradeUnknownAcceptedStartNeverRelaunches(t *testing.T) {
	f, pin, pending := upgradeMigrationStorageFixture(t)
	unknown := pending
	unknown.UpgradeGeneration.AcceptedStart = true
	f.client.options.DaemonStartGuard = daemonStartGuardFunc(func(context.Context, string, func() error) error {
		t.Fatal("unknown accepted Start was launched again")
		return ErrBinding
	})
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		if writeErr := replacePrivateMigrationRecordContext(ctx, f.base, pending, unknown, f.client.options.PrivateGenerationGuard); writeErr != nil {
			return writeErr
		}
		if validateErr := validatePrivateUpgradeMigration(ctx, pin, unknown); validateErr != nil {
			return validateErr
		}
		return f.client.completePrivateUpgradeMigration(ctx, f.root, f.base, unknown)
	})
	var pendingErr *PrivateUpgradeError
	if !errors.As(err, &pendingErr) || pendingErr.ReasonCode != "successor_birth_unknown" || !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown Start did not retain uncertainty: %v", err)
	}
}

func TestUpgradePinnedMissingTargetNeverReselects(t *testing.T) {
	f, pin, pending := upgradeMigrationStorageFixture(t)
	options := f.client.options
	options.DaemonWaitOwner = &upgradeNativeWaitOwner{}
	before := snapshotPrivateSuccessorTestState(t, f.root)
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		_, _, completeErr := CompletePrivateUpgradeWithLock(ctx, pin, pending.UpgradeGeneration.OperationID, func(context.Context) (string, string, error) {
			t.Fatal("pinned missing target was silently reselected")
			return "", "", ErrConfiguration
		}, options)
		return completeErr
	})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing pinned target did not fail explicitly: %v", err)
	}
	assertPrivateSuccessorTestStateUnchanged(t, f.root, before)
}

func TestUpgradeDrainCannotRetargetOrRewindAcceptedOperation(t *testing.T) {
	f, pin, pending := upgradeMigrationStorageFixture(t)
	drain, found, err := readPrivateUpgrade(pin, pending.UpgradeGeneration.OperationID)
	if err != nil || !found {
		t.Fatal(err)
	}
	before := readPrivateSupervisorFile(t, privateBootstrapRecordPath(f.root))
	retargeted := drain
	retargeted.SelectedPair.DaemonExecutable += "-other"
	rewound := drain
	rewound.Phase = UpgradeAuthorized
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		for _, candidate := range []privateUpgradeDrain{retargeted, rewound} {
			if writeErr := replacePrivateUpgrade(ctx, pin, drain, candidate, f.client.options); !errors.Is(writeErr, ErrBinding) {
				t.Fatalf("accepted operation changed authority: %v", writeErr)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(readPrivateSupervisorFile(t, privateBootstrapRecordPath(f.root))) {
		t.Fatal("refused retarget changed durable winner")
	}
}

func TestUpgradeGenericProgressCannotEraseAcceptedStart(t *testing.T) {
	f, _, pending := upgradeMigrationStorageFixture(t)
	accepted := pending
	accepted.UpgradeGeneration.AcceptedStart = true
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		if writeErr := replacePrivateMigrationRecordContext(ctx, f.base, pending, accepted, f.client.options.PrivateGenerationGuard); writeErr != nil {
			return writeErr
		}
		return replacePrivateMigrationRecordContext(ctx, f.base, accepted, pending, f.client.options.PrivateGenerationGuard)
	})
	if !errors.Is(err, ErrBinding) {
		t.Fatalf("generic write erased Start acceptance: %v", err)
	}
	winner, found, err := loadPrivateMigrationRecord(f.base)
	if err != nil || !found || winner != accepted {
		t.Fatalf("accepted Start was lost: %+v found=%t err=%v", winner, found, err)
	}
}

type upgradeSettlementFixture struct {
	owned, closed bool
	err           error
}

func (upgradeSettlementFixture) RegisterDaemonWait(string, int, <-chan struct{}) error {
	return ErrInfrastructureOwnerRequired
}

func (o upgradeSettlementFixture) ObserveDaemonWait(context.Context, string, lifetime.ProcessRef) (bool, bool, error) {
	return o.owned, o.closed, o.err
}

func TestUpgradePredecessorAbsenceRequiresKnownWaitSettlement(t *testing.T) {
	f, _, pending := upgradeMigrationStorageFixture(t)
	unknown := errors.New("fixture custody is unknown")
	for _, test := range []struct {
		name   string
		owner  DaemonWaitOwner
		want   error
		native bool
	}{
		{"missing", nil, ErrInfrastructureOwnerRequired, false},
		{"unknown", upgradeSettlementFixture{err: unknown}, unknown, false},
		{"pending", upgradeSettlementFixture{owned: true}, ErrPrivateUpgradePending, false},
		{"invalid", upgradeSettlementFixture{closed: true}, ErrBinding, false},
		{"known-none", upgradeSettlementFixture{}, nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := *f.client
			client.options.DaemonWaitOwner = test.owner
			observed := false
			client.options.processObserver = func(int) (privateProcessObservation, error) { observed = true; return privateProcessObservation{}, nil }
			requested := pending
			requested.LegacyStopped = false
			err := client.requirePrivateUpgradePredecessorClosed(t.Context(), f.base, requested)
			if !errors.Is(err, test.want) || observed != test.native {
				t.Fatalf("Wait settlement fallback: native=%t want=%t err=%v", observed, test.native, err)
			}
		})
	}
}

func TestUpgradeDurablePredecessorClosureSurvivesSuccessorCustody(t *testing.T) {
	f, _, pending := upgradeMigrationStorageFixture(t)
	f.client.options.DaemonWaitOwner = upgradeSettlementFixture{owned: true}
	f.client.options.processObserver = func(int) (privateProcessObservation, error) {
		t.Fatal("durable predecessor closure was reclassified through successor native state")
		return privateProcessObservation{}, ErrUnknown
	}
	if err := f.client.requirePrivateUpgradePredecessorClosed(t.Context(), f.base, pending); err != nil {
		t.Fatal(err)
	}
}
