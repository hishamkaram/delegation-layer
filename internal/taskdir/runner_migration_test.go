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

	digest := task.ComputeSHA256([]byte("verified managed runner"))
	must(t, td.RecordManagedRunnerMigrationContext(context.Background(), digest))
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
	record.MetaSHA256 = strings.Repeat("f", 64)
	data, err := task.MarshalCanonical(record)
	must(t, err)
	must(t, os.WriteFile(recordPath, data, 0o600))
	if _, err = td.HasManagedRunnerMigration(); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("migration receipt with a different metadata hash was accepted: %v", err)
	}
}
