package control

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// These stores contain only immutable records/config bytes. They launch no
// daemon and do not claim native process or completed-generation authority.
func TestInitialBootstrapEligibilityAllowsFreshAndOrdinaryInitialIntent(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "ordinary-initial"}[initial], func(t *testing.T) {
			root, _ := newPrivateSuccessorTestState(t)
			if err := os.Remove(filepath.Join(root, privateSupervisorDirectory, "bootstrap.lock")); err != nil {
				t.Fatal(err)
			}
			if initial {
				writePrivateSuccessorTestRecord(t, privateBootstrapRecordPath(root), privateBootstrapRecord{SchemaVersion: task.SchemaVersion})
				writePrivateSuccessorTestBytes(t, PrivateConfigPath(root), privateSuccessorTestConfig(root, "A"))
			}
			assertInitialBootstrapEligibility(t, root, true, nil)
		})
	}
	missing := filepath.Join(canonicalTemp(t), "not-provisioned")
	eligible, err := PrivateSupervisorInitialBootstrapEligible(missing)
	if err != nil || !eligible {
		t.Fatalf("absent authority was not initial-eligible: eligible=%t err=%v", eligible, err)
	}
	if _, err = os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("eligibility observation provisioned missing state: %v", err)
	}
}

func TestInitialBootstrapEligibilityRejectsPredecessorAuthority(t *testing.T) {
	for _, kind := range []string{
		"published-binding", "orphan-daemon-identity", "migration", "completed-history", "empty-history",
		"identity-publication", "recovery-alias", "recovery-publication", "orphan-recovery-config",
		"pair-restart", "completed-runtime-restart", "drain",
	} {
		t.Run(kind, func(t *testing.T) {
			root, bindings := newPrivateSuccessorTestState(t)
			seedInitialBootstrapPredecessor(t, root, bindings, kind)
			assertInitialBootstrapEligibility(t, root, false, nil)
		})
	}
}

func TestInitialBootstrapIntentCannotHideOrphanedPredecessor(t *testing.T) {
	for _, kind := range []string{"orphan-daemon-identity", "completed-history"} {
		t.Run(kind, func(t *testing.T) {
			root, bindings := newPrivateSuccessorTestState(t)
			writePrivateSuccessorTestRecord(t, privateBootstrapRecordPath(root), privateBootstrapRecord{SchemaVersion: task.SchemaVersion})
			seedInitialBootstrapPredecessor(t, root, bindings, kind)
			assertInitialBootstrapEligibility(t, root, false, nil)
		})
	}
}

func seedInitialBootstrapPredecessor(t *testing.T, root string, bindings map[string]task.SupervisorRef, kind string) {
	t.Helper()
	base := filepath.Join(root, privateSupervisorDirectory)
	record := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(bindings["A"]), Successor: privateDaemonIdentityForBinding(bindings["B"]), LegacyPID: 123, LegacyStopped: true}
	switch kind {
	case "published-binding":
		writePrivateSuccessorTestRecord(t, privateSupervisorBindingPath(root), privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: bindings["A"]})
	case "orphan-daemon-identity":
		writePrivateSuccessorTestRecord(t, privateDaemonIdentityPath(base), record.Legacy)
	case "migration":
		writePrivateSuccessorTestRecord(t, privateMigrationRecordPath(base), record)
	case "completed-history":
		writePrivateSuccessorTestRecord(t, privateMigrationHistoryPath(base), privateMigrationHistory{SchemaVersion: task.SchemaVersion, Migrations: []privateMigrationRecord{record}})
	case "empty-history":
		writePrivateSuccessorTestRecord(t, privateMigrationHistoryPath(base), privateMigrationHistory{SchemaVersion: task.SchemaVersion, Migrations: []privateMigrationRecord{}})
	case "identity-publication":
		writePrivateSuccessorTestRecord(t, privateIdentityPublicationPath(root), privateIdentityPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: bindings["A"]})
	case "recovery-alias", "recovery-publication", "orphan-recovery-config":
		seedInitialBootstrapRecovery(t, root, bindings["A"], kind)
	case "pair-restart", "completed-runtime-restart", "drain":
		seedInitialBootstrapControl(t, root, bindings["A"], kind)
	default:
		t.Fatalf("unknown predecessor fixture %q", kind)
	}
}

func seedInitialBootstrapRecovery(t *testing.T, root string, binding task.SupervisorRef, kind string) {
	t.Helper()
	data := privateSuccessorTestConfig(root, "A")
	record := privateRecoveryBindingRecord{SchemaVersion: task.SchemaVersion, Supervisor: binding, ConfigPath: privateRecoveryConfigPath(root), ConfigSHA256: task.ComputeSHA256(data)}
	switch kind {
	case "orphan-recovery-config":
		writePrivateSuccessorTestBytes(t, privateRecoveryConfigPath(root), data)
	case "recovery-publication":
		writePrivateSuccessorTestBytes(t, privateRecoveryPendingConfigPath(root), data)
		writePrivateSuccessorTestRecord(t, privateRecoveryPendingRecordPath(root), privateRecoveryPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: record})
	default:
		writePrivateSuccessorTestBytes(t, record.ConfigPath, data)
		writePrivateSuccessorTestRecord(t, privateRecoveryRecordPath(root), record)
	}
}

func seedInitialBootstrapControl(t *testing.T, root string, binding task.SupervisorRef, kind string) {
	t.Helper()
	record := privateBootstrapRecord{SchemaVersion: task.SchemaVersion}
	switch kind {
	case "pair-restart":
		target := binding
		target.ClientExecutable = filepath.Join(root, "new-install", "pueue")
		target.DaemonExecutable = filepath.Join(root, "new-install", "pueued")
		record.Restart = &privatePairRestartIntent{Predecessor: binding, Target: target}
	case "completed-runtime-restart":
		record.RuntimeRestart = &privateRuntimeRestartRecord{From: "bootstrap-observation-old", Binding: binding, Phase: RuntimeRestartReady, OldPID: 101, OldProcessStart: "recorded-old", Successor: "bootstrap-observation-new", NewPID: 102, NewProcessStart: "recorded-new"}
	case "drain":
		pin, err := readControlRootPin(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		record.DrainV1 = &privateUpgradeDrain{Root: upgradeRootSubject(pin), Predecessor: binding, OperationID: UpgradeOperationID("upgrade-v1:" + strings.Repeat("a", 32)), Mode: UpgradeOperationRequired, Phase: UpgradeDraining}
	}
	writePrivateSuccessorTestRecord(t, privateBootstrapRecordPath(root), record)
}

func TestInitialBootstrapEligibilityPreservesAuthorityReadFaults(t *testing.T) {
	for _, kind := range []string{"bootstrap", "published", "identity", "migration", "history", "identity-publication", "recovery-record", "recovery-config", "recovery-publication"} {
		t.Run(kind, func(t *testing.T) {
			root, bindings := newPrivateSuccessorTestState(t)
			paths := map[string]string{
				"bootstrap": privateBootstrapRecordPath(root), "published": privateSupervisorBindingPath(root),
				"identity":             privateDaemonIdentityPath(filepath.Join(root, privateSupervisorDirectory)),
				"migration":            privateMigrationRecordPath(filepath.Join(root, privateSupervisorDirectory)),
				"history":              privateMigrationHistoryPath(filepath.Join(root, privateSupervisorDirectory)),
				"identity-publication": privateIdentityPublicationPath(root), "recovery-record": privateRecoveryRecordPath(root), "recovery-config": privateRecoveryConfigPath(root),
				"recovery-publication": privateRecoveryPendingRecordPath(root),
			}
			if kind == "recovery-config" {
				seedInitialBootstrapRecovery(t, root, bindings["A"], "recovery-alias")
			}
			writePrivateSuccessorTestBytes(t, paths[kind], []byte("}"))
			assertInitialBootstrapEligibility(t, root, false, ErrBinding)
		})
	}
}

func TestInitialBootstrapEligibilityPreservesUnreadableRecoveryPublication(t *testing.T) {
	root, _ := newPrivateSuccessorTestState(t)
	makePrivateTestDirectory(t, privateRecoveryPendingRecordPath(root))
	assertInitialBootstrapEligibility(t, root, false, ErrConfiguration)
}

func TestInitialBootstrapEligibilityRetainsFaultsAlongsidePredecessor(t *testing.T) {
	root, bindings := newPrivateSuccessorTestState(t)
	seedInitialBootstrapPredecessor(t, root, bindings, "orphan-daemon-identity")
	writePrivateSuccessorTestRecord(t, privateBootstrapRecordPath(root), privateBootstrapRecord{SchemaVersion: task.SchemaVersion + 1})
	makePrivateTestDirectory(t, privateMigrationHistoryPath(filepath.Join(root, privateSupervisorDirectory)))
	before := snapshotPrivateSuccessorTestState(t, root)
	eligible, err := PrivateSupervisorInitialBootstrapEligible(root)
	if eligible || !errors.Is(err, task.ErrUnsupportedSchema) || !errors.Is(err, ErrConfiguration) || !errors.Is(err, ErrBinding) {
		t.Fatalf("existing authority hid required read faults: eligible=%t err=%v", eligible, err)
	}
	if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, root)) {
		t.Fatal("failed eligibility observation changed existing authority")
	}
}

func TestInitialBootstrapEligibilityRejectsInvalidRoot(t *testing.T) {
	for _, root := range []string{"", "relative-root", "/unclean/../root"} {
		eligible, err := PrivateSupervisorInitialBootstrapEligible(root)
		if eligible || !errors.Is(err, ErrConfiguration) {
			t.Fatalf("invalid root became bootstrap eligible: root=%q eligible=%t err=%v", root, eligible, err)
		}
	}
}

func assertInitialBootstrapEligibility(t *testing.T, root string, want bool, cause error) {
	t.Helper()
	before := snapshotPrivateSuccessorTestState(t, root)
	eligible, err := PrivateSupervisorInitialBootstrapEligible(root)
	if eligible != want || !errors.Is(err, cause) {
		t.Fatalf("initial bootstrap eligibility: eligible=%t want=%t err=%v wantCause=%v", eligible, want, err, cause)
	}
	if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, root)) {
		t.Fatal("passive eligibility observation changed persisted state")
	}
}
