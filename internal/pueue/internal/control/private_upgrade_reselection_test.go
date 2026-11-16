package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/migration"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// These real-store scenarios start no daemon. Finite authored control programs
// establish preparation failure and queue observations, not native lifetime.
type upgradeReselectionFixture struct {
	stored         generationGuardFixture
	pin            taskdir.RootPin
	op             UpgradeOperationID
	selected       privateUpgradePair
	options        Options
	selectionCalls int
	stop           error
	protected      map[string]privateSuccessorTestFile
	starts         atomic.Int32
	mutations      atomic.Int32
}

func newUpgradeReselectionFixture(t *testing.T) *upgradeReselectionFixture {
	t.Helper()
	stored, pin, accepted := upgradeJournalFixture(t, UpgradeOperationRequired)
	f := &upgradeReselectionFixture{stored: stored, pin: pin, op: accepted.OperationID, options: stored.client.options, stop: errors.New("fixture stops accepted migration before native effects")}
	f.options.DaemonWaitOwner = upgradeSettlementFixture{err: ErrUnknown}
	f.options.DaemonStartGuard = daemonStartGuardFunc(func(context.Context, string, func() error) error {
		f.starts.Add(1)
		return ErrInfrastructureOwnerRequired
	})
	f.options.Observer = f.observeCommand
	f.options.PrivateMigrationAuthorizer = f.authorizePreparation
	writePrivateSuccessorTestBytes(t, filepath.Join(stored.base, "ready"), nil)
	err := WithExistingBootstrapLockContext(t.Context(), stored.root, func(ctx context.Context) error {
		awaiting, advanceErr := AdvancePrivateUpgradeDrainWithLock(ctx, pin, f.op, f.options)
		if advanceErr != nil || awaiting.Phase != UpgradeAwaitingInstallation {
			return errors.Join(ErrBinding, advanceErr)
		}
		_, authorizeErr := AuthorizePrivateUpgradeCompletionWithLock(ctx, pin, f.op, f.options)
		return authorizeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	f.protected = upgradeReselectionProtectedState(t, stored.root)
	client, daemon := installPrivateSupervisorPair(t, filepath.Join(filepath.Dir(stored.root), "selected-install"))
	f.selected = privateUpgradePair{ClientExecutable: client, DaemonExecutable: daemon}
	writeExecutable(t, client, "#!/bin/sh\nexit 71\n")
	observed, prepareErr := f.complete(t, f.selected)
	if prepareErr == nil || errors.Is(prepareErr, os.ErrNotExist) || observed.Phase != UpgradeAuthorized || f.selectionCalls != 1 {
		t.Fatalf("fixture did not fail after pair selection: phase=%s selections=%d err=%v", observed.Phase, f.selectionCalls, prepareErr)
	}
	drain, found, readErr := readPrivateUpgrade(pin, f.op)
	if readErr != nil || !found || drain.SelectedPair != f.selected || drain.Phase != UpgradeAuthorized || drain.CompletionAuthorization.OperationID != f.op || drain.MigrationReference != (privateUpgradeOperationRef{}) {
		t.Fatalf("failed target preparation did not retain its exact authorized pair: found=%t drain=%+v err=%v", found, drain, readErr)
	}
	assertPrivateTestPathMissing(t, privateMigrationRecordPath(stored.base))
	assertPrivateTestPathMissing(t, privateRecoveryRecordPath(stored.root))
	assertPrivateTestPathMissing(t, privateRecoveryConfigPath(stored.root))
	f.assertNoNativeEffects(t)
	return f
}

func (f *upgradeReselectionFixture) observeCommand(event CommandEvent) {
	if event.Stage == "started" && slices.ContainsFunc(event.Argv, func(argument string) bool {
		return slices.Contains([]string{"shutdown", "kill", "remove", "clean", "add", "enqueue"}, argument)
	}) {
		f.mutations.Add(1)
	}
}

func (f *upgradeReselectionFixture) authorizePreparation(ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) error {
	if root != f.stored.root || binding != f.stored.client.Binding() || len(jobs) != 0 {
		return ErrBinding
	}
	if err := requireRuntimeRestartBootstrapEX(ctx, root); err != nil {
		return err
	}
	_, found, err := loadPrivateMigrationRecord(f.stored.base)
	if err != nil {
		return err
	}
	if found {
		// Stop before shutdown. The existing migration owner's failure cleanup
		// may still preserve its exact predecessor alias after acceptance.
		return f.stop
	}
	return nil
}

func (f *upgradeReselectionFixture) complete(t *testing.T, pair privateUpgradePair) (PrivateUpgradeObservation, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var observed PrivateUpgradeObservation
	err := WithExistingBootstrapLockContext(ctx, f.stored.root, func(locked context.Context) error {
		client, value, completeErr := CompletePrivateUpgradeWithLock(locked, f.pin, f.op, func(context.Context) (string, string, error) {
			f.selectionCalls++
			return pair.ClientExecutable, pair.DaemonExecutable, nil
		}, f.options)
		observed = value
		if client != nil {
			return errors.Join(completeErr, client.CloseAfterPending(completeErr))
		}
		return completeErr
	})
	return observed, err
}

func upgradeReselectionProtectedState(t *testing.T, root string) map[string]privateSuccessorTestFile {
	t.Helper()
	state := snapshotPrivateSuccessorTestState(t, root)
	for path, file := range state {
		if !file.mode.IsRegular() || path == privateBootstrapRecordPath(root) || path == privateMigrationRecordPath(filepath.Join(root, privateSupervisorDirectory)) || path == privateRecoveryRecordPath(root) || path == privateRecoveryConfigPath(root) {
			delete(state, path)
		}
	}
	return state
}

func (f *upgradeReselectionFixture) assertNoNativeEffects(t *testing.T) {
	t.Helper()
	if f.starts.Load() != 0 || f.mutations.Load() != 0 {
		t.Fatalf("preparation crossed the native effects boundary: starts=%d mutations=%d", f.starts.Load(), f.mutations.Load())
	}
	if !reflect.DeepEqual(f.protected, upgradeReselectionProtectedState(t, f.stored.root)) {
		t.Fatal("target preparation changed config, published identity or other protected records")
	}
	assertPrivateTestPathMissing(t, filepath.Join(f.stored.base, "daemon-started"))
}

func (f *upgradeReselectionFixture) assertPinnedMigration(t *testing.T, pair privateUpgradePair, observed PrivateUpgradeObservation, completeErr error) {
	t.Helper()
	if !errors.Is(completeErr, f.stop) || observed.Phase != UpgradeMigrating || observed.OperationID != f.op {
		t.Errorf("same operation did not reach the guarded migration boundary: phase=%s op=%s selections=%d err=%v", observed.Phase, observed.OperationID, f.selectionCalls, completeErr)
	}
	if f.assertAcceptedRecord(t, pair) {
		f.assertPreservedAlias(t)
	}
	f.assertNoNativeEffects(t)
}

func (f *upgradeReselectionFixture) assertAcceptedRecord(t *testing.T, pair privateUpgradePair) bool {
	t.Helper()
	record, found, err := loadPrivateMigrationRecord(f.stored.base)
	if err != nil || !found || record.UpgradeGeneration.OperationID != f.op || record.UpgradeGeneration.Target.ClientExecutable != pair.ClientExecutable || record.UpgradeGeneration.Target.DaemonExecutable != pair.DaemonExecutable {
		t.Errorf("same operation did not pin its selected migration target: found=%t selections=%d err=%v", found, f.selectionCalls, err)
	}
	if record.LegacyShutdownRequested || record.LegacyShutdownDispatched || record.LegacyStopped || record.UpgradeGeneration.AcceptedStart || record.UpgradeGeneration.SuccessorPID != 0 {
		t.Error("target acceptance invented native migration progress")
	}
	return found
}

func (f *upgradeReselectionFixture) assertPreservedAlias(t *testing.T) {
	t.Helper()
	alias, present, aliasErr := readValidatedPrivateRecoveryAlias(f.stored.root)
	if aliasErr != nil || !present || alias.record.Supervisor != f.stored.client.Binding() || !alias.matches(f.stored.client.Binding()) {
		t.Errorf("accepted migration did not preserve its exact predecessor alias: present=%t err=%v", present, aliasErr)
	}
}

func TestUpgradeAuthorizedMissingPairReselectsBeforeMigrationAcceptance(t *testing.T) {
	f := newUpgradeReselectionFixture(t)
	for _, path := range []string{f.selected.ClientExecutable, f.selected.DaemonExecutable} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	client, daemon := installPrivateSupervisorPair(t, filepath.Join(filepath.Dir(f.stored.root), "current-install"))
	current := privateUpgradePair{ClientExecutable: client, DaemonExecutable: daemon}
	observed, err := f.complete(t, current)
	if f.selectionCalls != 2 {
		t.Errorf("missing pre-migration pair permanently skipped current installation selection: selections=%d err=%v", f.selectionCalls, err)
	}
	f.assertPinnedMigration(t, current, observed, err)
}

func TestUpgradeAuthorizedPresentPairKeepsItsSelection(t *testing.T) {
	f := newUpgradeReselectionFixture(t)
	writeExecutable(t, f.selected.ClientExecutable, privateClientFixture)
	client, daemon := installPrivateSupervisorPair(t, filepath.Join(filepath.Dir(f.stored.root), "other-install"))
	observed, err := f.complete(t, privateUpgradePair{ClientExecutable: client, DaemonExecutable: daemon})
	if f.selectionCalls != 1 {
		t.Error("present selected installation was silently retargeted")
	}
	f.assertPinnedMigration(t, f.selected, observed, err)
}

func TestUpgradeAuthorizedReselectionDoesNotHideFaults(t *testing.T) {
	for _, fault := range []string{"existing target probe failure", "nonexecutable selected role", "corrupt migration record", "missing client with directory daemon"} {
		t.Run(fault, func(t *testing.T) {
			f := newUpgradeReselectionFixture(t)
			stageUpgradeReselectionFault(t, f, fault)
			before := snapshotPrivateSuccessorTestState(t, f.stored.root)
			_, err := f.complete(t, privateUpgradePair{})
			if err == nil || errors.Is(err, os.ErrNotExist) || f.selectionCalls != 1 {
				t.Fatalf("operational failure granted installation reselection: selections=%d err=%v", f.selectionCalls, err)
			}
			assertPrivateSuccessorTestStateUnchanged(t, f.stored.root, before)
			f.assertNoNativeEffects(t)
		})
	}
}

func stageUpgradeReselectionFault(t *testing.T, f *upgradeReselectionFixture, fault string) {
	t.Helper()
	switch fault {
	case "existing target probe failure":
	case "nonexecutable selected role":
		if err := os.Chmod(f.selected.ClientExecutable, 0o600); err != nil {
			t.Fatal(err)
		}
	case "corrupt migration record":
		writePrivateSuccessorTestBytes(t, privateMigrationRecordPath(f.stored.base), []byte("{"))
	case "missing client with directory daemon":
		removeUpgradeReselectionPair(t, f.selected)
		if err := os.Mkdir(f.selected.DaemonExecutable, 0o700); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown reselection fault fixture: %s", fault)
	}
}

func removeUpgradeReselectionPair(t *testing.T, pair privateUpgradePair) {
	t.Helper()
	for _, path := range []string{pair.ClientExecutable, pair.DaemonExecutable} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func upgradeReselectionRecord(t *testing.T, f *upgradeReselectionFixture) privateMigrationRecord {
	t.Helper()
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	target := f.stored.client.Binding()
	target.ClientExecutable, target.DaemonExecutable = f.selected.ClientExecutable, f.selected.DaemonExecutable
	return privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(f.stored.client.Binding()), Successor: privateDaemonIdentityForBinding(target), UpgradeGeneration: migration.UpgradeGeneration{OperationID: f.op, Root: upgradeRootSubject(f.pin), Predecessor: f.stored.client.Binding(), Mode: UpgradeOperationRequired, Target: target, LegacyBootID: boot}}
}

func TestUpgradeAuthorizedPublishedMigrationPreventsReselection(t *testing.T) {
	f := newUpgradeReselectionFixture(t)
	removeUpgradeReselectionPair(t, f.selected)
	record := upgradeReselectionRecord(t, f)
	err := WithExistingBootstrapLockContext(t.Context(), f.stored.root, func(ctx context.Context) error {
		return publishPrivateMigrationStateContext(ctx, f.stored.base, record, f.options.PrivateGenerationGuard)
	})
	if err != nil {
		t.Fatal(err)
	}
	drain, found, err := readPrivateUpgrade(f.pin, f.op)
	if err != nil || !found || drain.Phase != UpgradeAuthorized {
		t.Fatal("fixture did not preserve the publication-before-link interruption")
	}
	before := readPrivateSupervisorFile(t, privateMigrationRecordPath(f.stored.base))
	_, completeErr := f.complete(t, privateUpgradePair{})
	if !errors.Is(completeErr, os.ErrNotExist) || f.selectionCalls != 1 {
		t.Fatalf("published target was reselected before phase linking: selections=%d err=%v", f.selectionCalls, completeErr)
	}
	if string(before) != string(readPrivateSupervisorFile(t, privateMigrationRecordPath(f.stored.base))) {
		t.Fatal("missing published target changed its accepted migration record")
	}
	assertUpgradeReselectionWriterRefused(t, f)
	f.assertNoNativeEffects(t)
}

func assertUpgradeReselectionWriterRefused(t *testing.T, f *upgradeReselectionFixture) {
	t.Helper()
	drain, found, err := readPrivateUpgrade(f.pin, f.op)
	if err != nil || !found {
		t.Fatal(err)
	}
	client, daemon := installPrivateSupervisorPair(t, filepath.Join(filepath.Dir(f.stored.root), "replacement-install"))
	replacement := drain
	replacement.SelectedPair = privateUpgradePair{ClientExecutable: client, DaemonExecutable: daemon}
	before := snapshotPrivateSuccessorTestState(t, f.stored.root)
	err = WithExistingBootstrapLockContext(t.Context(), f.stored.root, func(ctx context.Context) error {
		return reselectPrivateUpgradePair(ctx, f.pin, drain, replacement, f.options)
	})
	if !errors.Is(err, ErrBinding) {
		t.Fatalf("direct writer bypassed durable authority: %v", err)
	}
	assertPrivateSuccessorTestStateUnchanged(t, f.stored.root, before)
}

func TestUpgradeReselectionRechecksMissingRoleAtCommit(t *testing.T) {
	for _, change := range []string{"recorded pair restored", "migration published"} {
		t.Run(change, func(t *testing.T) { testUpgradeReselectionCommitChange(t, change) })
	}
}

func testUpgradeReselectionCommitChange(t *testing.T, change string) {
	t.Helper()
	f := newUpgradeReselectionFixture(t)
	removeUpgradeReselectionPair(t, f.selected)
	client, daemon := installPrivateSupervisorPair(t, filepath.Join(filepath.Dir(f.stored.root), "current-install"))
	record := upgradeReselectionRecord(t, f)
	guardCalls := 0
	var guardedBefore map[string]privateSuccessorTestFile
	f.options.PrivateGenerationGuard = generationGuardFunc(func(_ context.Context, _ string, commit func() error) error {
		guardCalls++
		// Change canonical evidence only after candidate discovery. The writer
		// must repeat eligibility inside this commit owner before replacement.
		if change == "recorded pair restored" {
			writeExecutable(t, f.selected.ClientExecutable, privateClientFixture)
			writeExecutable(t, f.selected.DaemonExecutable, privateDaemonFixture)
		} else if err := publishPrivateMigrationState(f.stored.base, record); err != nil {
			return err
		}
		guardedBefore = snapshotPrivateSuccessorTestState(t, f.stored.root)
		return commit()
	})
	_, err := f.complete(t, privateUpgradePair{ClientExecutable: client, DaemonExecutable: daemon})
	if !errors.Is(err, ErrBinding) || guardCalls != 1 || f.selectionCalls != 2 {
		t.Fatalf("commit-time change did not fence reselection: guards=%d selections=%d err=%v", guardCalls, f.selectionCalls, err)
	}
	assertPrivateSuccessorTestStateUnchanged(t, f.stored.root, guardedBefore)
	f.assertNoNativeEffects(t)
}

func TestUpgradeReselectionWriterPreservesDurableAuthority(t *testing.T) {
	for _, state := range []string{"changed published predecessor", "completed operation history", "identity publication pending", "malformed history"} {
		t.Run(state, func(t *testing.T) {
			f := newUpgradeReselectionFixture(t)
			removeUpgradeReselectionPair(t, f.selected)
			stageUpgradeReselectionAuthority(t, f, state)
			f.protected = upgradeReselectionProtectedState(t, f.stored.root)
			assertUpgradeReselectionWriterRefused(t, f)
			f.assertNoNativeEffects(t)
		})
	}
}

// Canonical DATA below intentionally combines a stale Authorized drain with
// independently valid recovery records. It grants no native closure or birth.
func stageUpgradeReselectionAuthority(t *testing.T, f *upgradeReselectionFixture, state string) {
	t.Helper()
	if state == "malformed history" {
		writePrivateSuccessorTestBytes(t, privateMigrationHistoryPath(f.stored.base), []byte("{"))
		return
	}
	if state == "changed published predecessor" {
		changed := f.stored.client.Binding()
		changed.ObservedVersion += " refreshed"
		data, err := task.MarshalCanonical(privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: changed})
		if err != nil {
			t.Fatal(err)
		}
		writePrivateSuccessorTestBytes(t, privateSupervisorBindingPath(f.stored.root), data)
		return
	}
	if state == "identity publication pending" {
		data, err := task.MarshalCanonical(privateIdentityPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: f.stored.client.Binding()})
		if err != nil {
			t.Fatal(err)
		}
		writePrivateSuccessorTestBytes(t, privateIdentityPublicationPath(f.stored.root), data)
		return
	}
	record := upgradeReselectionRecord(t, f)
	record.Successor = record.Legacy
	record.UpgradeGeneration.Target = f.stored.client.Binding()
	record.LegacyPID, record.LegacyProcessStart = 101, "historical-predecessor"
	record.LegacyShutdownRequested, record.LegacyShutdownDispatched, record.LegacyStopped = true, true, true
	record.UpgradeGeneration.AcceptedStart = true
	record.UpgradeGeneration.SuccessorBootID = record.UpgradeGeneration.LegacyBootID
	record.UpgradeGeneration.SuccessorPID, record.UpgradeGeneration.SuccessorProcessStart = 102, "historical-successor"
	record.UpgradeGeneration.Completed = true
	if err := appendPrivateMigrationHistory(f.stored.base, record); err != nil {
		t.Fatal(err)
	}
	assertPrivateTestPathMissing(t, privateMigrationRecordPath(f.stored.base))
}

func TestUpgradeReselectionShapeCannotChangeAuthority(t *testing.T) {
	f := newUpgradeReselectionFixture(t)
	drain, found, err := readPrivateUpgrade(f.pin, f.op)
	if err != nil || !found {
		t.Fatal(err)
	}
	replacement := drain
	replacement.SelectedPair.DaemonExecutable += "-current"
	if err = validatePrivateUpgradeReselection(drain, replacement); err != nil {
		t.Fatalf("pair-only reselection shape was rejected: %v", err)
	}
	for _, change := range []struct {
		name   string
		mutate func(*privateUpgradeDrain)
	}{
		{"operation", func(d *privateUpgradeDrain) { d.OperationID += "different" }},
		{"root", func(d *privateUpgradeDrain) { d.Root.ID += "different" }},
		{"predecessor", func(d *privateUpgradeDrain) { d.Predecessor.ObservedVersion += "different" }},
		{"mode", func(d *privateUpgradeDrain) { d.Mode = UpgradeReactive }},
		{"authorization", func(d *privateUpgradeDrain) { d.CompletionAuthorization.OperationID += "different" }},
		{"phase", func(d *privateUpgradeDrain) { d.Phase = UpgradeMigrating }},
		{"migration reference", func(d *privateUpgradeDrain) { d.MigrationReference.OperationID = d.OperationID }},
		{"zero pair", func(d *privateUpgradeDrain) { d.SelectedPair = privateUpgradePair{} }},
		{"unchanged pair", func(d *privateUpgradeDrain) { d.SelectedPair = drain.SelectedPair }},
	} {
		t.Run(change.name, func(t *testing.T) {
			candidate := replacement
			change.mutate(&candidate)
			if err := validatePrivateUpgradeReselection(drain, candidate); !errors.Is(err, ErrBinding) {
				t.Fatalf("reselection changed immutable authority: %v", err)
			}
		})
	}
}

func TestUpgradeReselectionPreservesPreparedCallbackAlias(t *testing.T) {
	f := newUpgradeReselectionFixture(t)
	writeExecutable(t, f.selected.ClientExecutable, privateClientFixture)
	f.options.PrivateTerminalEventRunner = privateInstalledCallbackForTest(t, f.stored.root)
	configPath := f.stored.client.Binding().ConfigPath
	original := readPrivateSupervisorFile(t, configPath)
	interrupted := errors.New("fixture callback config committed before acknowledgement")
	f.options.PrivateGenerationGuard = generationGuardFunc(func(_ context.Context, _ string, commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		if string(original) != string(readPrivateSupervisorFile(t, configPath)) {
			return interrupted
		}
		return nil
	})
	_, err := f.complete(t, f.selected)
	if !errors.Is(err, interrupted) || f.selectionCalls != 1 {
		t.Fatalf("fixture did not interrupt after callback preparation: selections=%d err=%v", f.selectionCalls, err)
	}
	assertPrivateTestPathMissing(t, privateMigrationRecordPath(f.stored.base))
	prepared := readPrivateSupervisorFile(t, configPath)
	aliasRecord := readPrivateSupervisorFile(t, privateRecoveryRecordPath(f.stored.root))
	aliasConfig := readPrivateSupervisorFile(t, privateRecoveryConfigPath(f.stored.root))
	alias, present, err := readValidatedPrivateRecoveryAlias(f.stored.root)
	if err != nil || !present || !alias.matches(f.stored.client.Binding()) || string(original) != string(aliasConfig) || string(original) == string(prepared) {
		t.Fatalf("callback interruption lacks canonical predecessor/config evidence: present=%t err=%v", present, err)
	}
	f.protected = upgradeReselectionProtectedState(t, f.stored.root)
	f.options.PrivateGenerationGuard = allowPrivateGenerationForTest{}
	removeUpgradeReselectionPair(t, f.selected)
	client, daemon := installPrivateSupervisorPair(t, filepath.Join(filepath.Dir(f.stored.root), "current-install"))
	current := privateUpgradePair{ClientExecutable: client, DaemonExecutable: daemon}
	observed, err := f.complete(t, current)
	if f.selectionCalls != 2 {
		t.Errorf("callback-prepared operation did not reselect its missing installation: selections=%d err=%v", f.selectionCalls, err)
	}
	f.assertPinnedMigration(t, current, observed, err)
	if string(aliasRecord) != string(readPrivateSupervisorFile(t, privateRecoveryRecordPath(f.stored.root))) || string(aliasConfig) != string(readPrivateSupervisorFile(t, privateRecoveryConfigPath(f.stored.root))) || string(prepared) != string(readPrivateSupervisorFile(t, configPath)) {
		t.Fatal("same-operation retry reset or replaced callback preparation evidence")
	}
}

func TestUpgradeReselectionPostcommitFaultKeepsCanonicalWinner(t *testing.T) {
	f := newUpgradeReselectionFixture(t)
	removeUpgradeReselectionPair(t, f.selected)
	client, daemon := installPrivateSupervisorPair(t, filepath.Join(filepath.Dir(f.stored.root), "current-install"))
	current := privateUpgradePair{ClientExecutable: client, DaemonExecutable: daemon}
	postcommit := errors.New("fixture lost acknowledgement after pair replacement")
	f.options.PrivateGenerationGuard = generationGuardFunc(func(_ context.Context, _ string, commit func() error) error {
		return errors.Join(commit(), postcommit)
	})
	_, err := f.complete(t, current)
	if !errors.Is(err, postcommit) || f.selectionCalls != 2 {
		t.Fatalf("fixture did not retain postcommit replacement failure: selections=%d err=%v", f.selectionCalls, err)
	}
	drain, found, readErr := readPrivateUpgrade(f.pin, f.op)
	if readErr != nil || !found || drain.SelectedPair != current || drain.Phase != UpgradeAuthorized || drain.MigrationReference != (privateUpgradeOperationRef{}) {
		t.Fatalf("replacement acknowledgement fault lost its canonical winner: found=%t drain=%+v err=%v", found, drain, readErr)
	}
	assertPrivateTestPathMissing(t, privateMigrationRecordPath(f.stored.base))
	f.assertNoNativeEffects(t)
	f.options.PrivateGenerationGuard = allowPrivateGenerationForTest{}
	observed, err := f.complete(t, privateUpgradePair{})
	if f.selectionCalls != 2 {
		t.Error("canonical replacement winner was reselected after lost acknowledgement")
	}
	f.assertPinnedMigration(t, current, observed, err)
}

func TestUpgradeAcceptedStartMissingPairNeverReselects(t *testing.T) {
	f, pin, record := upgradeMigrationStorageFixture(t)
	accepted := record
	accepted.UpgradeGeneration.AcceptedStart = true
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		return replacePrivateMigrationRecordContext(ctx, f.base, record, accepted, f.client.options.PrivateGenerationGuard)
	})
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotPrivateSuccessorTestState(t, f.root)
	options := f.client.options
	options.DaemonWaitOwner = upgradeSettlementFixture{err: ErrUnknown}
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		_, _, completeErr := CompletePrivateUpgradeWithLock(ctx, pin, record.UpgradeGeneration.OperationID, func(context.Context) (string, string, error) {
			t.Error("accepted Start permitted target reselection")
			return "", "", ErrConfiguration
		}, options)
		return completeErr
	})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("accepted missing target did not retain its explicit fault: %v", err)
	}
	assertPrivateSuccessorTestStateUnchanged(t, f.root, before)
}
