package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// configDriftRecoveryAlias mirrors the private recovery alias record.
type configDriftRecoveryAlias struct {
	SchemaVersion int                `json:"schema_version"`
	Supervisor    task.SupervisorRef `json:"supervisor"`
	ConfigPath    string             `json:"config_path"`
	ConfigSHA256  string             `json:"config_sha256"`
}

func configDriftAliasPaths(root string) (string, string) {
	base := filepath.Dir(pueue.PrivateConfigPath(root))
	return filepath.Join(base, "legacy-supervisor.json"), filepath.Join(base, "legacy-pueue.yml")
}

// The blocked migration prepared the exact predecessor-to-successor journal
// and has not recorded any predecessor stop progress.
func assertConfigDriftPreparedJournal(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	var journal historicalPrivateMigrationRecord
	readHistoricalRecord(t, filepath.Join(fixture.root, ".supervisor", "migration.json"), &journal)
	config := readSupervisorProbeTestFile(t, pueue.PrivateConfigPath(fixture.root))
	daemonB := readSupervisorProbeTestFile(t, fixture.daemonB)
	successor := journal.Successor
	if journal.Legacy != historicalPrivateDaemonIdentityFor(fixture.predecessor) {
		t.Fatalf("journal predecessor = %+v, want %+v", journal.Legacy, fixture.predecessor)
	}
	if successor.ConfigPath != pueue.PrivateConfigPath(fixture.root) || successor.ConfigDigest != task.ComputeSHA256(config) || !strings.HasSuffix(successor.Endpoint, "upgrade.sock") || successor.SHA256 != task.ComputeSHA256(daemonB) {
		t.Fatalf("journal successor does not select the drifted config and installed daemon: %+v", successor)
	}
	if journal.LegacyPID != 0 || journal.LegacyProcessStart != "" || journal.LegacyShutdownRequested || journal.LegacyShutdownDispatched || journal.LegacyStopped {
		t.Fatalf("blocked journal recorded predecessor stop progress: %+v", journal)
	}
}

// The alias names the original predecessor, but its own bytes are a
// reserialized copy: the raw digest differs from the predecessor's original
// config digest, so only the effective-config proof can match them.
func assertConfigDriftReserializedAlias(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	recordPath, configPath := configDriftAliasPaths(fixture.root)
	var alias configDriftRecoveryAlias
	readHistoricalRecord(t, recordPath, &alias)
	bytes := readSupervisorProbeTestFile(t, configPath)
	if alias.Supervisor != fixture.predecessor || alias.ConfigPath != configPath || alias.ConfigSHA256 != task.ComputeSHA256(bytes) {
		t.Fatalf("alias is not the predecessor bound to its own bytes: %+v", alias)
	}
	if alias.ConfigSHA256 == fixture.predecessor.ConfigDigest {
		t.Fatal("alias holds the original config bytes; the test does not exercise reserialization")
	}
}

// Alias retirement keeps its completion fence: the prepared journal refuses
// retirement even though the predecessor phase is now observable.
func assertConfigDriftAliasRetirementRefused(t *testing.T, fixture *historicalSupervisorFixture) {
	t.Helper()
	err := pueue.WithBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		return pueue.RetirePrivateRecoveryBindingContext(ctx, fixture.root, fixture.predecessor)
	})
	if !errors.Is(err, pueue.ErrBinding) || !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) || !strings.Contains(err.Error(), "not ready to retire its recovery alias") {
		t.Fatalf("prepared drifted journal did not keep the alias retirement fence: %v", err)
	}
}

// configDriftDurableFiles lists every file the public consumers must leave
// byte-identical, including each saved task file.
func configDriftDurableFiles(t *testing.T, fixture *historicalSupervisorFixture) []string {
	t.Helper()
	recordPath, configPath := configDriftAliasPaths(fixture.root)
	paths := []string{filepath.Join(fixture.root, ".supervisor", "migration.json"), recordPath, configPath, fixture.daemonStartLog, filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")}
	err := filepath.WalkDir(fixture.td.Dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && entry.Type().IsRegular() {
			paths = append(paths, path)
		}
		return walkErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
