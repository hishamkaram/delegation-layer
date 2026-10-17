package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestMigratePrivateSupervisorRecoversMissingLogicalBindingFromLivePredecessor(t *testing.T) {
	base, stateRoot := newPrivateSupervisorTestRoots(t)
	clientPath := filepath.Join(base, "pueue")
	oldDaemonPath := filepath.Join(base, "old-pueued")
	currentDaemonPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, clientPath, privateClientFixture)
	writeExecutable(t, oldDaemonPath, privateDaemonFixture)
	writeExecutable(t, currentDaemonPath, privateDaemonFixture+"\n# current bundle\n")
	options := privateMigrationTestOptions(stateRoot, privateTestObservationTimeout)
	if _, err := BindPrivate(context.Background(), clientPath, oldDaemonPath, stateRoot, options); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(privateSupervisorBindingPath(stateRoot)); err != nil {
		t.Fatal(err)
	}
	current, err := BindPrivate(context.Background(), clientPath, currentDaemonPath, stateRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = WithBootstrapLock(context.Background(), stateRoot, func() error {
		return current.MigratePrivateSupervisorWithLock(context.Background(), stateRoot)
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
	writePrivateSuccessorTestRecord(t, identityPath, privateDaemonIdentity{
		Executable: legacyBinding.DaemonExecutable,
		SHA256:     legacyBinding.DaemonSHA256,
	})
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
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, currentDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	if err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return RetirePrivateRecoveryBinding(fixture.stateRoot, successor)
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
	if err := RetirePrivateRecoveryBindingContext(ctx, fixture.stateRoot, legacy.Binding()); !errors.Is(err, context.Canceled) {
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
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, currentDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	if err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return RetirePrivateRecoveryBinding(fixture.stateRoot, successor)
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
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, currentDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	recordBefore := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.stateRoot))
	configBefore := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot))
	unrelated := successor
	unrelated.DaemonExecutable = canonicalPrivateTestExecutable(t, otherDaemon)
	unrelated.DaemonSHA256 = hashPrivateTestExecutable(t, otherDaemon)
	if err := WithBootstrapLock(context.Background(), fixture.stateRoot, func() error {
		return RetirePrivateRecoveryBinding(fixture.stateRoot, unrelated)
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
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, currentDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	historyPath := privateMigrationHistoryPath(filepath.Join(fixture.stateRoot, privateSupervisorDirectory))
	history, found, err := loadPrivateMigrationHistory(filepath.Dir(historyPath))
	if err != nil || !found {
		t.Fatalf("successor history was not seeded: found=%t err=%v", found, err)
	}
	history.Migrations[0].Successor.Executable = canonicalPrivateTestExecutable(t, otherDaemon)
	history.Migrations[0].Successor.SHA256 = hashPrivateTestExecutable(t, otherDaemon)
	writePrivateSuccessorTestRecord(t, historyPath, history)
	recordBefore := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.stateRoot))
	configBefore := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.stateRoot))
	if err := RetirePrivateRecoveryBinding(fixture.stateRoot, successor); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
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
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, successorDaemon)
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
	if err := RetirePrivateRecoveryBinding(fixture.stateRoot, successor); !errors.Is(err, ErrBinding) {
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
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, successorDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	if err := os.Remove(privateRecoveryRecordPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	if err := RetirePrivateRecoveryBinding(fixture.stateRoot, successor); err != nil {
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
	if err := RetirePrivateRecoveryBinding(fixture.stateRoot, legacy.Binding()); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
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
	successor.DaemonSHA256 = hashPrivateTestExecutable(t, successorDaemon)
	seedPrivateRecoveryRetirementSuccessor(t, fixture.stateRoot, legacy.Binding(), successor)
	if err := os.Remove(privateRecoveryConfigPath(fixture.stateRoot)); err != nil {
		t.Fatal(err)
	}
	if err := RetirePrivateRecoveryBinding(fixture.stateRoot, successor); err != nil {
		t.Fatalf("partially removed recovery record was not retired: %v", err)
	}
	for _, path := range []string{privateRecoveryRecordPath(fixture.stateRoot), privateRecoveryConfigPath(fixture.stateRoot)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial recovery artifact remained at %s: %v", path, err)
		}
	}
}

func TestSelectRecoveryDaemonUsesFallbackAfterSavedBytesChange(t *testing.T) {
	base := canonicalTemp(t)
	savedPath := filepath.Join(base, "saved-pueued")
	fallbackPath := filepath.Join(base, "current-pueued")
	writeExecutable(t, savedPath, "#!/bin/sh\nold\n")
	writeExecutable(t, fallbackPath, "#!/bin/sh\ncurrent\n")
	savedDigest, err := hashExecutable(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(savedPath, []byte("#!/bin/sh\nreplaced\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	saved := task.SupervisorRef{DaemonExecutable: savedPath, DaemonSHA256: savedDigest}
	got, digest, err := selectRecoveryDaemon(saved, fallbackPath)
	if err != nil {
		t.Fatalf("select recovery daemon failed: %v", err)
	}
	want, err := canonicalExecutable(fallbackPath)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, err := hashExecutable(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != want || digest != wantDigest {
		t.Fatalf("fallback daemon = (%q, %q), want (%q, %q)", got, digest, want, wantDigest)
	}
}
