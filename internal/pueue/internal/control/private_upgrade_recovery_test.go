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

type upgradeAcceptanceFaultGuard struct {
	base            string
	mode            string
	acceptanceFault error
	rollbackFault   error
	injected        bool
	afterAcceptance func() error
}

func (g *upgradeAcceptanceFaultGuard) GuardPrivateGenerationCommit(_ context.Context, _ string, write func() error) error {
	before, found, err := loadPrivateMigrationRecord(g.base)
	if err != nil || !found {
		return errors.Join(ErrBinding, err)
	}
	if g.mode == "prewrite" && !g.injected {
		g.injected = true
		return g.acceptanceFault
	}
	if g.injected && before.UpgradeGeneration.AcceptedStart {
		switch g.mode {
		case "rollback-fault":
			return g.rollbackFault
		case "revoked":
			return ErrInfrastructureOwnerRequired
		}
	}
	return g.afterWrite(before, write())
}

func (g *upgradeAcceptanceFaultGuard) afterWrite(before privateMigrationRecord, writeErr error) error {
	after, found, readErr := loadPrivateMigrationRecord(g.base)
	if readErr != nil || !found {
		return errors.Join(writeErr, ErrBinding, readErr)
	}
	if !g.injected && !before.UpgradeGeneration.AcceptedStart && after.UpgradeGeneration.AcceptedStart {
		g.injected = true
		if g.afterAcceptance != nil {
			return errors.Join(writeErr, g.acceptanceFault, g.afterAcceptance())
		}
		if g.mode == "revoked" {
			return errors.Join(writeErr, g.acceptanceFault, ErrInfrastructureOwnerRequired)
		}
		return errors.Join(writeErr, g.acceptanceFault)
	}
	return writeErr
}

func TestUpgradeAcceptanceWriteFaultRetainsOnlyUnknownStart(t *testing.T) {
	for _, test := range []struct {
		name          string
		accepted      bool
		retry         bool
		rollbackFault bool
	}{
		{"prewrite", false, true, false},
		{"postcommit", false, true, false},
		{"rollback-fault", true, false, true},
		{"revoked", true, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			testUpgradeAcceptanceWriteFault(t, test.name, test.accepted, test.retry, test.rollbackFault)
		})
	}
}

func testUpgradeAcceptanceWriteFault(t *testing.T, mode string, wantAccepted, retry, wantRollbackFault bool) {
	t.Helper()
	f, pin, pending := upgradeMigrationStorageFixture(t)
	writeExecutable(t, pending.UpgradeGeneration.Target.DaemonExecutable, "#!/bin/sh\nexit 0\n")
	guard := &upgradeAcceptanceFaultGuard{base: f.base, mode: mode, acceptanceFault: errors.New("acceptance-specific postcommit fault"), rollbackFault: errors.New("unstarted acceptance rollback refused")}
	options := f.client.options
	options.PrivateGenerationGuard = guard
	client, owner := newUpgradeStartFixtureClient(t, pending.UpgradeGeneration.Target, options)
	paths := []string{client.Binding().ConfigPath, privateBootstrapRecordPath(f.root), privateSupervisorBindingPath(f.root), privateDaemonIdentityPath(f.base), privateIdentityPublicationPath(f.root)}
	before := snapshotPrivateTestFiles(t, paths)
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		_, startErr := client.startPrivateUpgradeSuccessor(ctx, pin, f.base, pending)
		assertUpgradeAcceptanceFault(t, guard, owner, startErr, wantRollbackFault)
		current, found, readErr := loadPrivateMigrationRecord(f.base)
		if readErr != nil || !found {
			return errors.Join(ErrBinding, readErr)
		}
		if current.UpgradeGeneration.AcceptedStart != wantAccepted {
			t.Fatalf("known no-child acceptance was stranded: accepted=%t want=%t", current.UpgradeGeneration.AcceptedStart, wantAccepted)
		}
		want := pending
		want.UpgradeGeneration.AcceptedStart = wantAccepted
		if current != want {
			t.Fatal("acceptance settlement changed operation, target, predecessor or shutdown proof")
		}
		for _, path := range paths {
			data, present := before[path]
			assertPrivateTestFileUnchanged(t, path, data, present)
		}
		if retry {
			assertUpgradeAcceptanceExplicitRetry(t, ctx, client, owner, pin, f.base, current)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertUpgradeAcceptanceFault(t *testing.T, guard *upgradeAcceptanceFaultGuard, owner *upgradeNativeWaitOwner, err error, wantRollbackFault bool) {
	t.Helper()
	if !errors.Is(err, guard.acceptanceFault) || !guard.injected || len(owner.starts) != 0 {
		t.Fatalf("failed acceptance reached Start or hid fault: starts=%d injected=%t err=%v", len(owner.starts), guard.injected, err)
	}
	if wantRollbackFault && !errors.Is(err, guard.rollbackFault) {
		t.Fatalf("acceptance error lost rollback fault: %v", err)
	}
	if guard.mode == "revoked" && !errors.Is(err, ErrInfrastructureOwnerRequired) {
		t.Fatalf("revoked grant was hidden: %v", err)
	}
}

func TestUpgradeAcceptanceFaultNeverResetsUnreadableOrConflictingWinner(t *testing.T) {
	for _, mode := range []string{"unreadable", "conflicting"} {
		t.Run(mode, func(t *testing.T) { testUpgradeAcceptanceUnknownWinner(t, mode) })
	}
}

func testUpgradeAcceptanceUnknownWinner(t *testing.T, mode string) {
	t.Helper()
	f, pin, pending := upgradeMigrationStorageFixture(t)
	writeExecutable(t, pending.UpgradeGeneration.Target.DaemonExecutable, "#!/bin/sh\nexit 0\n")
	fault := errors.New("acceptance landed before canonical winner fault")
	guard := &upgradeAcceptanceFaultGuard{base: f.base, mode: mode, acceptanceFault: fault}
	guard.afterAcceptance = func() error { return injectUpgradeAcceptanceWinnerFault(t, f.base, mode) }
	options := f.client.options
	options.PrivateGenerationGuard = guard
	client, owner := newUpgradeStartFixtureClient(t, pending.UpgradeGeneration.Target, options)
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		_, startErr := client.startPrivateUpgradeSuccessor(ctx, pin, f.base, pending)
		return startErr
	})
	if !errors.Is(err, fault) || !errors.Is(err, ErrBinding) || len(owner.starts) != 0 {
		t.Fatalf("unknown winner gained reset/Start authority or lost fault: starts=%d err=%v", len(owner.starts), err)
	}
	assertUpgradeAcceptanceUnknownWinnerRetained(t, f.base, mode, pending)
}

func assertUpgradeAcceptanceUnknownWinnerRetained(t *testing.T, base, mode string, pending privateMigrationRecord) {
	t.Helper()
	if mode == "unreadable" {
		info, err := os.Stat(privateMigrationRecordPath(base))
		if err != nil || !info.IsDir() {
			t.Fatalf("settlement replaced unreadable winner: %v", err)
		}
		return
	}
	winner, found, err := loadPrivateMigrationRecord(base)
	pending.UpgradeGeneration.AcceptedStart = true
	pending.LegacyProcessStart += "-conflicting-owner"
	if err != nil || !found || winner != pending {
		t.Fatalf("settlement changed conflicting winner: found=%t err=%v", found, err)
	}
}

func injectUpgradeAcceptanceWinnerFault(t *testing.T, base, mode string) error {
	t.Helper()
	path := privateMigrationRecordPath(base)
	if mode == "unreadable" {
		backup := filepath.Join(base, "acceptance-winner-before-unreadable.json")
		if err := os.Rename(path, backup); err != nil {
			return err
		}
		t.Cleanup(func() {
			removeErr := os.Remove(path)
			if errors.Is(removeErr, os.ErrNotExist) {
				removeErr = nil
			}
			if err := errors.Join(removeErr, os.Rename(backup, path)); err != nil {
				t.Error(err)
			}
		})
		return os.Mkdir(path, 0o700)
	}
	winner, found, err := loadPrivateMigrationRecord(base)
	if err != nil || !found {
		return errors.Join(ErrBinding, err)
	}
	conflicting := winner
	conflicting.LegacyProcessStart += "-conflicting-owner"
	return replacePrivateMigrationRecord(base, winner, conflicting)
}

func assertUpgradeAcceptanceExplicitRetry(t *testing.T, ctx context.Context, client *Client, owner *upgradeNativeWaitOwner, pin taskdir.RootPin, base string, pending privateMigrationRecord) {
	t.Helper()
	client.options.PrivateGenerationGuard = allowPrivateGenerationForTest{}
	started, err := client.startPrivateUpgradeSuccessor(ctx, pin, base, pending)
	if err != nil || !started.UpgradeGeneration.AcceptedStart || started.UpgradeGeneration.SuccessorPID <= 0 || len(owner.starts) != 1 {
		t.Fatalf("explicit retry did not own one Start/birth: starts=%d err=%v", len(owner.starts), err)
	}
	if _, err = client.startPrivateUpgradeSuccessor(ctx, pin, base, started); err == nil || len(owner.starts) != 1 {
		t.Fatalf("accepted retry replayed Start: starts=%d err=%v", len(owner.starts), err)
	}
}

func TestUpgradeStalePreAcceptanceCannotResetCrashAcceptance(t *testing.T) {
	f, pin, pending := upgradeMigrationStorageFixture(t)
	writeExecutable(t, pending.UpgradeGeneration.Target.DaemonExecutable, "#!/bin/sh\nexit 0\n")
	client, owner := newUpgradeStartFixtureClient(t, pending.UpgradeGeneration.Target, f.client.options)
	unknown := pending
	unknown.UpgradeGeneration.AcceptedStart = true
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		if writeErr := replacePrivateMigrationRecordContext(ctx, f.base, pending, unknown, f.client.options.PrivateGenerationGuard); writeErr != nil {
			return writeErr
		}
		_, startErr := client.startPrivateUpgradeSuccessor(ctx, pin, f.base, pending)
		if startErr == nil || len(owner.starts) != 0 {
			t.Fatalf("stale pre-acceptance launched a child: starts=%d err=%v", len(owner.starts), startErr)
		}
		winner, found, readErr := loadPrivateMigrationRecord(f.base)
		if readErr != nil || !found || winner != unknown {
			t.Fatalf("stale live call erased prior unknown acceptance: found=%t err=%v", found, readErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
