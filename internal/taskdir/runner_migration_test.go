package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestManagedRunnerMigrationIsBoundToImmutableTaskRecords(t *testing.T) {
	store := testStore(t)
	req, meta, brief := preparedInput(t, store)
	runner := filepath.Join(t.TempDir(), "delegate-run")
	meta.RunnerExecutable = runner
	td, err := store.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	defer func() { must(t, td.Close()) }()

	must(t, td.RecordManagedRunnerMigrationContext(context.Background()))
	recorded, readErr := td.HasManagedRunnerMigration()
	if readErr != nil || !recorded {
		t.Fatalf("managed runner migration receipt = %v, err=%v", recorded, readErr)
	}
	_, savedMeta, err := td.PreparedRecords()
	must(t, err)
	if savedMeta.RunnerOwnership != "" || savedMeta.RunnerExecutable != runner {
		t.Fatalf("migration changed immutable task metadata: %+v", savedMeta)
	}

	recordPath := filepath.Join(td.Dir, managedRunnerMigrationRecordName)
	var record managedRunnerMigrationRecord
	must(t, store.readRecord(recordPath, &record))
	if record.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || record.RunnerSHA256 != "" {
		t.Fatalf("new migration receipt retained executable byte identity: %+v", record)
	}
	record.MetaSHA256 = strings.Repeat("f", 64)
	data, err := task.MarshalCanonical(record)
	must(t, err)
	must(t, os.WriteFile(recordPath, data, 0o600))
	if _, err = td.HasManagedRunnerMigration(); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("migration receipt with a different metadata hash was accepted: %v", err)
	}
}

func TestManagedRunnerMigrationPreservesHistoricalReceipt(t *testing.T) {
	store := testStore(t)
	req, meta, brief := preparedInput(t, store)
	meta.RunnerExecutable = filepath.Join(t.TempDir(), "historical-runner")
	td, err := store.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	defer func() { must(t, td.Close()) }()
	_, _, specSHA, metaSHA, err := td.loadAndValidatePreparedSetContext(context.Background())
	must(t, err)
	legacy := managedRunnerMigrationRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID,
		TaskID: td.TaskID, SpecSHA256: specSHA, MetaSHA256: metaSHA,
		RunnerExecutable: meta.RunnerExecutable, RunnerSHA256: strings.Repeat("a", 64),
	}
	before, err := task.MarshalCanonical(legacy)
	must(t, err)
	path := filepath.Join(td.Dir, managedRunnerMigrationRecordName)
	must(t, os.WriteFile(path, before, 0o600))
	must(t, td.RecordManagedRunnerMigrationContext(context.Background()))
	after, err := os.ReadFile(path)
	must(t, err)
	if string(before) != string(after) {
		t.Fatal("ownership replay rewrote historical receipt")
	}
}

func TestManagedRunnerMigrationRejectsMixedExecutableRepresentations(t *testing.T) {
	record := managedRunnerMigrationRecord{
		SchemaVersion: task.SchemaVersion, RootID: strings.Repeat("a", 32),
		TaskID: strings.Repeat("b", 32), SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64),
		RunnerExecutable: filepath.Join(t.TempDir(), "delegate-run"), ExecutableIdentity: task.ExecutableIdentityInstalledPathV1,
	}
	if err := validateManagedRunnerMigrationRecord(record); err != nil {
		t.Fatal(err)
	}
	for _, identity := range []task.ExecutableIdentity{"", task.ExecutableIdentityInstalledPathV1, "unknown"} {
		invalid := record
		invalid.ExecutableIdentity = identity
		if identity == task.ExecutableIdentityInstalledPathV1 {
			invalid.RunnerSHA256 = strings.Repeat("a", 64)
		}
		if err := validateManagedRunnerMigrationRecord(invalid); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("invalid receipt identity %q accepted: %v", identity, err)
		}
	}
}
