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

func TestMigratePrivateSupervisorRecoversMissingLogicalBindingFromLivePredecessor(t *testing.T) {
	for _, removeInstallation := range []bool{false, true} {
		name := "installed predecessor"
		if removeInstallation {
			name = "removed predecessor installation"
		}
		t.Run(name, func(t *testing.T) {
			assertMissingLogicalBindingMigration(t, removeInstallation)
		})
	}
}

func assertMissingLogicalBindingMigration(t *testing.T, removeInstallation bool) {
	t.Helper()
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldInstallation := filepath.Join(base, "predecessor")
	oldClientPath, oldDaemonPath := installPrivateSupervisorPair(t, oldInstallation)
	clientPath, currentDaemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "current"))
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	waitOwner, joinDaemons := ownPrivateRecoveryDaemonWaits(t, stateRoot)
	options.DaemonWaitOwner = waitOwner
	legacy := bindPrivateSetupForTest(t, stateRoot, oldClientPath, oldDaemonPath, options)
	ownPrivateMigrationTestClient(t, legacy)
	options = legacy.options // Carry the actual daemon Wait owner into every launch.
	legacyBinding := legacy.Binding()
	legacyIdentity := readPrivateDaemonIdentity(t, stateRoot)
	if err := os.Remove(privateSupervisorBindingPath(stateRoot)); err != nil {
		t.Fatal(err)
	}
	current := preparePrivateCandidateForTest(t, stateRoot, clientPath, currentDaemonPath, options)
	if removeInstallation {
		// Close joins the predecessor's control work before removing its CLI.
		if err := legacy.Close(); err != nil {
			t.Fatal(err)
		}
		joinDaemons()
		removePrivateTestPath(t, oldInstallation)
	}
	if got := readPrivateDaemonIdentity(t, stateRoot); got != legacyIdentity {
		t.Fatal("candidate preparation changed the predecessor's persisted identity")
	}
	if err := WithBootstrapLockContext(t.Context(), stateRoot, func(ctx context.Context) error {
		return current.MigratePrivateSupervisorWithLock(ctx, stateRoot)
	}); err != nil {
		t.Fatalf("migration did not recover missing logical binding: %v", err)
	}
	persisted, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !task.SameSupervisorIdentity(persisted, current.Binding()) {
		t.Fatalf("successor logical binding was not published: found=%t binding=%+v want=%+v", found, persisted, current.Binding())
	}
	assertRecoveredPrivateMigrationPredecessor(t, stateRoot, legacyBinding)
	assertPrivateDaemonStartCount(t, stateRoot, 2)
}

// The finite authored daemons publish readiness and exit. Retain their actual
// startDaemon Wait completions, including on assertion failure, before any
// owned installation or state path is removed.
func ownPrivateRecoveryDaemonWaits(t *testing.T, stateRoot string) (DaemonWaitOwner, func()) {
	t.Helper()
	var waits []<-chan struct{}
	join := func() {
		for _, done := range waits {
			<-done
		}
	}
	t.Cleanup(func() {
		join()
		// These markers belong to the finite authored fixture, whose actual
		// processes have now exited. They are not native termination evidence.
		removePrivateTestDaemonState(t, stateRoot)
	})
	return daemonWaitOwnerFunc(func(_ string, _ int, done <-chan struct{}) error {
		waits = append(waits, done)
		return nil
	}), join
}

func ownPrivateMigrationTestClient(t *testing.T, client *Client) {
	t.Helper()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
}

func assertRecoveredPrivateMigrationPredecessor(t *testing.T, stateRoot string, legacy task.SupervisorRef) {
	t.Helper()
	history, found, err := loadPrivateMigrationHistory(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil || !found || len(history.Migrations) != 1 {
		t.Fatalf("recovered predecessor history: found=%t history=%+v err=%v", found, history, err)
	}
	if got := history.Migrations[0].Legacy; got != privateDaemonIdentityForBinding(legacy) {
		t.Fatalf("migration replaced the predecessor's saved daemon coordinates or logical facts: got=%+v want=%+v", got, privateDaemonIdentityForBinding(legacy))
	}
}

func TestMissingLogicalBindingRejectsMismatchedPendingPredecessorBeforePublication(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldClient, oldDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "predecessor"))
	clientPath, daemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "selected"))
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	waitOwner, _ := ownPrivateRecoveryDaemonWaits(t, stateRoot)
	options.DaemonWaitOwner = waitOwner
	legacy := bindPrivateSetupForTest(t, stateRoot, oldClient, oldDaemon, options)
	ownPrivateMigrationTestClient(t, legacy)
	current := preparePrivateCandidateForTest(t, stateRoot, clientPath, daemonPath, legacy.options)
	pending := privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(legacy.Binding()),
		Successor:     privateDaemonIdentityForBinding(current.Binding()),
	}
	pending.Legacy.ConfigDigest = task.ComputeSHA256([]byte("another predecessor config"))
	privateBase := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := publishPrivateMigrationState(privateBase, pending); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(privateSupervisorBindingPath(stateRoot)); err != nil {
		t.Fatal(err)
	}
	before := snapshotPrivateSuccessorTestState(t, stateRoot)
	err := WithBootstrapLockContext(t.Context(), stateRoot, func(ctx context.Context) error {
		return current.MigratePrivateSupervisorWithLock(ctx, stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("mismatched pending predecessor was accepted: %v", err)
	}
	if after := snapshotPrivateSuccessorTestState(t, stateRoot); !reflect.DeepEqual(before, after) {
		t.Fatal("mismatched pending predecessor published or changed durable generation state")
	}
	assertPrivateDaemonStartCount(t, stateRoot, 1)
}

func TestMissingLogicalBindingRejectsUnmarkedHistoricalDaemonWithoutFullReference(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	oldClient, oldDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "predecessor"))
	clientPath, daemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "selected"))
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	waitOwner, _ := ownPrivateRecoveryDaemonWaits(t, stateRoot)
	options.DaemonWaitOwner = waitOwner
	legacy := bindPrivateSetupForTest(t, stateRoot, oldClient, oldDaemon, options)
	ownPrivateMigrationTestClient(t, legacy)
	current := preparePrivateCandidateForTest(t, stateRoot, clientPath, daemonPath, legacy.options)
	identity := privateDaemonIdentityForBinding(legacy.Binding())
	// A frozen historical digest is decoded record data. It proves no installed
	// bytes and supplies neither the absent old client nor resolver provenance.
	_, historical := installedIdentityLegacyRef(t)
	identity.ExecutableIdentity = ""
	identity.SHA256 = historical.DaemonSHA256
	writePrivateSuccessorTestRecord(t, privateDaemonIdentityPath(filepath.Join(stateRoot, privateSupervisorDirectory)), identity)
	if err := os.Remove(privateSupervisorBindingPath(stateRoot)); err != nil {
		t.Fatal(err)
	}
	before := snapshotPrivateSuccessorTestState(t, stateRoot)
	err := WithBootstrapLockContext(t.Context(), stateRoot, func(ctx context.Context) error {
		return current.MigratePrivateSupervisorWithLock(ctx, stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("unmarked historical daemon supplied a complete predecessor binding: %v", err)
	}
	if after := snapshotPrivateSuccessorTestState(t, stateRoot); !reflect.DeepEqual(before, after) {
		t.Fatal("incomplete historical predecessor was relabeled or published")
	}
	assertPrivateDaemonStartCount(t, stateRoot, 1)
}

func TestMigratePrivateSupervisorRejectsChangedConfigWithoutLogicalPredecessorProof(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	legacy, err := BindPrivate(context.Background(), clientPath, oldDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	legacyBinding := legacy.Binding()
	t.Cleanup(func() {
		removePrivateTestDaemonState(t, stateRoot)
	})
	identityPath := privateDaemonIdentityPath(filepath.Join(stateRoot, privateSupervisorDirectory))
	writePrivateSuccessorTestRecord(t, identityPath, privateDaemonIdentityForBinding(legacyBinding))
	configData, err := os.ReadFile(legacyBinding.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	configData = append(configData, []byte("# changed logical generation\n")...)
	if writeErr := os.WriteFile(legacyBinding.ConfigPath, configData, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if removeErr := os.Remove(privateSupervisorBindingPath(stateRoot)); removeErr != nil {
		t.Fatal(removeErr)
	}
	current, err := Bind(context.Background(), clientPath, legacyBinding.ConfigPath, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := attachPrivateDaemon(current, currentDaemonPath); err != nil {
		t.Fatal(err)
	}
	identityBefore := readPrivateSupervisorFile(t, identityPath)
	configBefore := readPrivateSupervisorFile(t, legacyBinding.ConfigPath)
	if err := WithBootstrapLock(context.Background(), stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
	}); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("changed config was accepted without durable predecessor provenance: %v", err)
	}
	assertPrivateDaemonStartCount(t, stateRoot, 1)
	if got := readPrivateSupervisorFile(t, identityPath); !bytes.Equal(got, identityBefore) {
		t.Fatal("changed-config refusal rewrote daemon provenance")
	}
	if got := readPrivateSupervisorFile(t, legacyBinding.ConfigPath); !bytes.Equal(got, configBefore) {
		t.Fatal("changed-config refusal rewrote the saved config")
	}
	if _, err := os.Stat(privateSupervisorBindingPath(stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed-config refusal published a logical binding: %v", err)
	}
}

func TestMigratePrivateSupervisorRejectsSemanticallyChangedCompleteIdentityWithoutRelabeling(t *testing.T) {
	fixture := newSemanticallyChangedPrivateMigrationFixture(t)
	t.Cleanup(func() {
		removePrivateTestDaemonState(t, fixture.stateRoot)
	})

	bindingPath := privateSupervisorBindingPath(fixture.stateRoot)
	historyPath := privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))
	bindingBefore, bindingPresentBefore := snapshotPrivateTestFile(t, bindingPath)
	historyBefore, historyPresentBefore := snapshotPrivateTestFile(t, historyPath)
	identityBefore := readPrivateSupervisorFile(t, fixture.identityPath)
	configBefore := readPrivateSupervisorFile(t, fixture.configPath)
	err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return fixture.current.MigratePrivateSupervisorWithLock(context.Background(), fixture.stateRoot)
	})
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("live daemon response through changed generation was accepted as predecessor proof: %v", err)
	}
	assertPrivateTestFileUnchanged(t, bindingPath, bindingBefore, bindingPresentBefore)
	assertPrivateTestFileUnchanged(t, historyPath, historyBefore, historyPresentBefore)
	if got := readPrivateSupervisorFile(t, fixture.identityPath); !bytes.Equal(got, identityBefore) {
		t.Fatal("semantic-generation refusal changed daemon provenance")
	}
	if got := readPrivateSupervisorFile(t, fixture.configPath); !bytes.Equal(got, configBefore) {
		t.Fatal("semantic-generation refusal changed the current config")
	}
}

func newSemanticallyChangedPrivateMigrationFixture(t *testing.T) semanticallyChangedPrivateMigrationFixture {
	t.Helper()
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	daemonPath := filepath.Join(base, "pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, daemonPath, privateDaemonFixture)
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	legacy, bindErr := BindPrivate(context.Background(), clientPath, daemonPath, stateRoot, options)
	if bindErr != nil {
		t.Fatal(bindErr)
	}
	legacyBinding := legacy.Binding()
	identityPath := privateDaemonIdentityPath(filepath.Join(stateRoot, privateSupervisorDirectory))
	writePrivateSuccessorTestRecord(t, identityPath, privateDaemonIdentityForBinding(legacyBinding))
	configPath := legacyBinding.ConfigPath
	configData := readPrivateSupervisorFile(t, configPath)
	const showConfirmation = "  show_confirmation_questions: false\n"
	semanticConfig := strings.Replace(string(configData), showConfirmation, showConfirmation+"  edit_mode: files\n", 1)
	if semanticConfig == string(configData) {
		t.Fatal("test fixture did not find the client configuration anchor")
	}
	if writeErr := os.WriteFile(configPath, []byte(semanticConfig), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if removeErr := os.Remove(privateSupervisorBindingPath(stateRoot)); removeErr != nil {
		t.Fatal(removeErr)
	}
	current, currentErr := Bind(context.Background(), clientPath, configPath, options)
	if currentErr != nil {
		t.Fatal(currentErr)
	}
	if attachErr := attachPrivateDaemon(current, daemonPath); attachErr != nil {
		t.Fatal(attachErr)
	}
	if task.SameSupervisorIdentity(current.Binding(), legacyBinding) {
		t.Fatal("semantic config change did not create a distinct logical generation")
	}
	if current.Binding().Endpoint != legacyBinding.Endpoint {
		t.Fatalf("semantic config fixture changed endpoint: current=%+v legacy=%+v", current.Binding(), legacyBinding)
	}
	ready, readyErr := current.availableForBootstrap(context.Background())
	if readyErr != nil || !ready {
		t.Fatalf("live predecessor was not reachable through the changed generation: ready=%t err=%v", ready, readyErr)
	}
	return semanticallyChangedPrivateMigrationFixture{stateRoot: stateRoot, configPath: configPath, identityPath: identityPath, legacy: legacy, current: current}
}

func TestRetirePrivateRecoveryBindingRemovesStaleAliasAfterSuccessorReady(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	currentDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "current-pueued")
	writeExecutable(t, currentDaemon, privateDaemonFixture+"\n# successor\n")
	successor := legacy.Binding()
	successor.ConfigDigest = task.ComputeSHA256([]byte("successor-config"))
	successor.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("successor-resolved-config"))
	successor.Endpoint = "unix:/successor.sock"
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, currentDaemon)
	successor.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	successor.ClientSHA256, successor.DaemonSHA256 = "", ""
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	if err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return RetirePrivateRecoveryBindingWithOptionsContext(t.Context(), fixture.stateRoot, successor, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}})
	}); err != nil {
		t.Fatalf("stale recovery alias was not retired: %v", err)
	}
	if _, err := os.Stat(privateRecoveryRecordPath(fixture.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale recovery record remained after retirement: %v", err)
	}
}

func TestRetirePrivateRecoveryBindingContextRejectsCanceledContextWithoutMutation(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	paths := []string{privateRecoveryRecordPath(fixture.stateRoot), privateRecoveryConfigPath(fixture.stateRoot)}
	before := snapshotPrivateTestFiles(t, paths)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RetirePrivateRecoveryBindingWithOptionsContext(ctx, fixture.stateRoot, legacy.Binding(), Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled alias retirement error = %v, want context cancellation", err)
	}
	after := snapshotPrivateTestFiles(t, paths)
	for path, want := range before {
		if got, found := after[path]; !found || !bytes.Equal(got, want) {
			t.Fatalf("canceled alias retirement changed %s", path)
		}
	}
}

func TestRetirePrivateRecoveryBindingRemovesAliasForSameSuccessor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	currentDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "same-logical-successor-pueued")
	writeExecutable(t, currentDaemon, privateDaemonFixture+"\n# same logical successor\n")
	successor := legacy.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, currentDaemon)
	successor.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	successor.ClientSHA256, successor.DaemonSHA256 = "", ""
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	if err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return RetirePrivateRecoveryBindingWithOptionsContext(t.Context(), fixture.stateRoot, successor, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}})
	}); err != nil {
		t.Fatalf("same-identity recovery alias was not retired: %v", err)
	}
	if _, err := os.Stat(privateRecoveryRecordPath(fixture.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("same-identity recovery record remained after retirement: %v", err)
	}
}

func TestRetirePrivateRecoveryBindingRejectsUnrelatedSuccessor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	currentDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "current-pueued")
	otherDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "other-pueued")
	writeExecutable(t, currentDaemon, privateDaemonFixture+"\n# current successor\n")
	writeExecutable(t, otherDaemon, privateDaemonFixture+"\n# unrelated successor\n")
	successor := legacy.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, currentDaemon)
	successor.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	successor.ClientSHA256, successor.DaemonSHA256 = "", ""
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	recordBefore := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.stateRoot))
	configBefore := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot))
	unrelated := successor
	unrelated.DaemonExecutable = canonicalPrivateTestExecutable(t, otherDaemon)
	unrelated.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	unrelated.ClientSHA256, unrelated.DaemonSHA256 = "", ""
	if err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return RetirePrivateRecoveryBindingWithOptionsContext(t.Context(), fixture.stateRoot, unrelated, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}})
	}); err == nil {
		t.Fatal("unrelated successor was allowed to retire the active recovery alias")
	}
	if _, err := os.Stat(privateRecoveryRecordPath(fixture.stateRoot)); err != nil {
		t.Fatalf("recovery record was removed after unrelated retirement attempt: %v", err)
	}
	if _, err := os.Stat(privateRecoveryConfigPath(fixture.stateRoot)); err != nil {
		t.Fatalf("recovery config was removed after unrelated retirement attempt: %v", err)
	}
	if got := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.stateRoot)); !bytes.Equal(got, recordBefore) {
		t.Fatal("unrelated retirement changed the recovery record")
	}
	if got := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot)); !bytes.Equal(got, configBefore) {
		t.Fatal("unrelated retirement changed the recovery config")
	}
}

func TestRetirePrivateRecoveryBindingRejectsMismatchedSuccessorHistory(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	currentDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "history-current-pueued")
	otherDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "history-other-pueued")
	writeExecutable(t, currentDaemon, privateDaemonFixture+"\n# current successor\n")
	writeExecutable(t, otherDaemon, privateDaemonFixture+"\n# history mismatch\n")
	successor := legacy.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, currentDaemon)
	successor.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	successor.ClientSHA256, successor.DaemonSHA256 = "", ""
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	historyPath := privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))
	history, found, err := loadPrivateMigrationHistory(filepath.Dir(historyPath))
	if err != nil || !found {
		t.Fatalf("successor history was not seeded: found=%t err=%v", found, err)
	}
	history.Migrations[0].Successor.Executable = canonicalPrivateTestExecutable(t, otherDaemon)
	history.Migrations[0].Successor.Executable = canonicalPrivateTestExecutable(t, otherDaemon)
	writePrivateSuccessorTestRecord(t, historyPath, history)
	recordBefore := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.stateRoot))
	configBefore := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot))
	if err := RetirePrivateRecoveryBindingWithOptionsContext(t.Context(), fixture.stateRoot, successor, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}}); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("mismatched successor history was accepted: %v", err)
	}
	if got := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.stateRoot)); !bytes.Equal(got, recordBefore) {
		t.Fatal("mismatched successor history changed the recovery record")
	}
	if got := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot)); !bytes.Equal(got, configBefore) {
		t.Fatal("mismatched successor history changed the recovery config")
	}
}

func TestRetirePrivateRecoveryBindingRejectsIncompletePredecessorShutdown(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	successorDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "incomplete-successor-pueued")
	writeExecutable(t, successorDaemon, privateDaemonFixture+"\n# incomplete-history successor\n")
	successor := legacy.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, successorDaemon)
	successor.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	successor.ClientSHA256, successor.DaemonSHA256 = "", ""
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	historyPath := privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))
	writePrivateSuccessorTestRecord(t, historyPath, privateMigrationHistory{
		SchemaVersion: task.SchemaVersion,
		Migrations: []privateMigrationRecord{{
			SchemaVersion: task.SchemaVersion,
			Legacy:        privateDaemonIdentityForBinding(legacy.Binding()),
			Successor:     privateDaemonIdentityForBinding(successor),
			LegacyPID:     101,
			LegacyStopped: false,
		}},
	})

	paths := []string{
		privateRecoveryConfigPath(fixture.stateRoot),
		privateRecoveryRecordPath(fixture.stateRoot),
		privateRecoveryPendingConfigPath(fixture.stateRoot),
		privateRecoveryPendingRecordPath(fixture.stateRoot),
	}
	before := snapshotPrivateTestFiles(t, paths)
	if err := RetirePrivateRecoveryBindingWithOptionsContext(t.Context(), fixture.stateRoot, successor, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}}); !errors.Is(err, ErrBinding) {
		t.Fatalf("incomplete predecessor shutdown history was accepted: %v", err)
	}
	after := snapshotPrivateTestFiles(t, paths)
	if len(after) != len(before) {
		t.Fatalf("incomplete-history refusal changed recovery alias artifact set: before=%d after=%d", len(before), len(after))
	}
	for path, want := range before {
		got, ok := after[path]
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("incomplete-history refusal changed recovery alias artifact %s", path)
		}
	}
}

func seedPrivateRecoveryRetirementSuccessor(t *testing.T, stateRoot string, legacy, successor task.SupervisorRef) {
	t.Helper()
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := publishPrivateDaemonIdentity(base, successor); err != nil {
		t.Fatal(err)
	}
	if err := appendPrivateMigrationHistory(base, privateMigrationRecord{
		SchemaVersion: task.SchemaVersion,
		Legacy:        privateDaemonIdentityForBinding(legacy),
		Successor:     privateDaemonIdentityForBinding(successor),
		LegacyPID:     101,
		LegacyStopped: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRetirePrivateRecoveryBindingRetriesOrphanedConfig(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	successorDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "orphan-successor-pueued")
	writeExecutable(t, successorDaemon, privateDaemonFixture+"\n# orphan cleanup successor\n")
	successor := legacy.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, successorDaemon)
	successor.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	successor.ClientSHA256, successor.DaemonSHA256 = "", ""
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	if err := os.Remove(privateRecoveryRecordPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	if err := RetirePrivateRecoveryBindingWithOptionsContext(t.Context(), fixture.stateRoot, successor, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}}); err != nil {
		t.Fatalf("orphaned recovery config was not retired: %v", err)
	}
	if _, err := os.Stat(privateRecoveryConfigPath(fixture.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned recovery config remained after retirement: %v", err)
	}
}

func TestRetirePrivateRecoveryBindingRejectsAmbiguousOrphanedConfig(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(privateRecoveryRecordPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	configBefore := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot))
	if err := RetirePrivateRecoveryBindingWithOptionsContext(t.Context(), fixture.stateRoot, legacy.Binding(), Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}}); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("ambiguous orphaned recovery config was retired: %v", err)
	}
	if got := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot)); !bytes.Equal(got, configBefore) {
		t.Fatal("ambiguous orphaned recovery config changed")
	}
}

func TestRetirePrivateRecoveryBindingCleansPartiallyRemovedRecord(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	legacy := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	if err := legacy.PersistPrivateRecoveryBinding(fixture.stateRoot); err != nil {
		t.Fatal(err)
	}
	successorDaemon := filepath.Join(filepath.Dir(fixture.daemonPath), "partial-successor-pueued")
	writeExecutable(t, successorDaemon, privateDaemonFixture+"\n# partial cleanup successor\n")
	successor := legacy.Binding()
	successor.DaemonExecutable = canonicalPrivateTestExecutable(t, successorDaemon)
	successor.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	successor.ClientSHA256, successor.DaemonSHA256 = "", ""
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	if err := os.Remove(privateRecoveryConfigPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	if err := RetirePrivateRecoveryBindingWithOptionsContext(t.Context(), fixture.stateRoot, successor, Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}}); err != nil {
		t.Fatalf("partially removed recovery record was not retired: %v", err)
	}
	for _, path := range []string{privateRecoveryRecordPath(fixture.stateRoot), privateRecoveryConfigPath(fixture.stateRoot)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial recovery artifact remained at %s: %v", path, err)
		}
	}
}

func TestSelectRecoveryDaemonKeepsRecordedInstalledPathAfterCompatibleUpdate(t *testing.T) {
	base := canonicalTemp(t)
	savedPath, fallbackPath := filepath.Join(base, "saved-pueued"), filepath.Join(base, "current-pueued")
	writeExecutable(t, savedPath, "#!/bin/sh\nexit 0\n")
	writeExecutable(t, fallbackPath, "#!/bin/sh\nexit 0\n")
	saved := task.SupervisorRef{ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, DaemonExecutable: savedPath}
	writeExecutable(t, savedPath, "#!/bin/sh\n# compatible installed update\nexit 0\n")
	got, err := selectRecoveryDaemon(saved, fallbackPath)
	if err != nil || got != savedPath {
		t.Fatalf("recorded daemon selection = %q, %v", got, err)
	}
	if err = os.Remove(savedPath); err != nil {
		t.Fatal(err)
	}
	got, err = selectRecoveryDaemon(saved, fallbackPath)
	if err != nil || got != fallbackPath {
		t.Fatalf("absent recorded daemon fallback = %q, %v", got, err)
	}
	if err = os.Mkdir(savedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err = selectRecoveryDaemon(saved, fallbackPath); err == nil {
		t.Fatal("invalid recorded role silently fell back")
	}
}

func TestMissingLogicalBindingResumesStoppedSuccessor(t *testing.T) {
	base, root := newPrivateSupervisorTestRoots(t)
	oldClient, oldDaemon := installPrivateSupervisorPair(t, filepath.Join(base, "predecessor"))
	clientPath, daemonPath := installPrivateSupervisorPair(t, filepath.Join(base, "successor"))
	options := privateMigrationTestOptions(root, privateTestObservationTimeout)
	waitOwner, _ := ownPrivateRecoveryDaemonWaits(t, root)
	options.DaemonWaitOwner = waitOwner
	legacy := bindPrivateSetupForTest(t, root, oldClient, oldDaemon, options)
	ownPrivateMigrationTestClient(t, legacy)
	current := preparePrivateCandidateForTest(t, root, clientPath, daemonPath, legacy.options)
	if err := WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		return current.MigratePrivateSupervisorWithLock(ctx, root)
	}); err != nil {
		t.Fatal(err)
	}
	privateBase := filepath.Join(root, privateSupervisorDirectory)
	history, found, err := loadPrivateMigrationHistory(privateBase)
	if err != nil || !found || len(history.Migrations) != 1 {
		t.Fatalf("completed migration history: %+v found=%t err=%v", history, found, err)
	}
	if err := WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		if err := publishPrivateMigrationStateContext(ctx, privateBase, history.Migrations[0], legacy.options.PrivateGenerationGuard); err != nil {
			return err
		}
		if err := os.Remove(privateSupervisorBindingPath(root)); err != nil {
			return err
		}
		return current.MigratePrivateSupervisorWithLock(ctx, root)
	}); err != nil {
		t.Fatalf("stopped successor could not recover its missing logical binding: %v", err)
	}
	assertPrivateDaemonIdentityMatchesBinding(t, root, current.Binding())
	assertPrivateDaemonStartCount(t, root, 2)
	if pending, err := PrivateSupervisorMigrationPending(root); err != nil || pending {
		t.Fatalf("stopped replay retained migration journal: pending=%t err=%v", pending, err)
	}
}
