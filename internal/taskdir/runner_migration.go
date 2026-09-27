package taskdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const managedRunnerMigrationRecordName = "runner-migration.json"

type managedRunnerMigrationRecord struct {
	SchemaVersion    int    `json:"schema_version"`
	RootID           string `json:"root_id"`
	TaskID           string `json:"task_id"`
	SpecSHA256       string `json:"spec_sha256"`
	MetaSHA256       string `json:"meta_sha256"`
	RunnerExecutable string `json:"runner_executable"`
	RunnerSHA256     string `json:"runner_sha256"`
}

// RecordManagedRunnerMigration preserves verified managed ownership for a
// legacy task whose immutable metadata predates RunnerOwnership.
func (td *TaskDir) RecordManagedRunnerMigrationContext(ctx context.Context, runnerSHA256 string) (resultErr error) {
	if td == nil || td.store == nil {
		return task.ErrInvalidPermit
	}
	if ctx == nil {
		return context.Canceled
	}
	if err := td.store.maintLock.LockSH(ctx); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.store.maintLock.Unlock()) }()
	_, _, meta, specSHA256, metaSHA256, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return err
	}
	if meta.RunnerOwnership != "" || meta.RunnerExecutable == "" || task.ValidateSHA256(runnerSHA256) != nil {
		return task.ErrIdentityMismatch
	}
	record := managedRunnerMigrationRecord{
		SchemaVersion:    task.SchemaVersion,
		RootID:           td.store.RootID,
		TaskID:           td.TaskID,
		SpecSHA256:       specSHA256,
		MetaSHA256:       metaSHA256,
		RunnerExecutable: meta.RunnerExecutable,
		RunnerSHA256:     runnerSHA256,
	}
	return td.recordSameContext(ctx, managedRunnerMigrationRecordName, record, func() (bool, error) {
		old, readErr := td.readManagedRunnerMigration()
		return readErr == nil && reflect.DeepEqual(old, &record), readErr
	})
}

// HasManagedRunnerMigration validates the migration receipt against the
// immutable task records before exposing its ownership decision.
func (td *TaskDir) HasManagedRunnerMigration() (bool, error) {
	_, err := td.readManagedRunnerMigration()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (td *TaskDir) readManagedRunnerMigration() (*managedRunnerMigrationRecord, error) {
	if td == nil || td.store == nil {
		return nil, task.ErrInvalidPermit
	}
	_, _, meta, specSHA256, metaSHA256, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, err
	}
	var record managedRunnerMigrationRecord
	if err = td.store.readRecord(filepath.Join(td.Dir, managedRunnerMigrationRecordName), &record); err != nil {
		return nil, err
	}
	if err = validateManagedRunnerMigrationRecord(record); err != nil {
		return nil, err
	}
	if err = matchManagedRunnerMigrationRecord(record, td, meta, specSHA256, metaSHA256); err != nil {
		return nil, err
	}
	return &record, nil
}

func validateManagedRunnerMigrationRecord(record managedRunnerMigrationRecord) error {
	if record.SchemaVersion != task.SchemaVersion || task.ValidateRootID(record.RootID) != nil || task.ValidateTaskID(record.TaskID) != nil || task.ValidateSHA256(record.SpecSHA256) != nil || task.ValidateSHA256(record.MetaSHA256) != nil || task.ValidateSHA256(record.RunnerSHA256) != nil {
		return fmt.Errorf("%w: invalid managed runner migration record", task.ErrEvidenceFault)
	}
	if !filepath.IsAbs(record.RunnerExecutable) || filepath.Clean(record.RunnerExecutable) != record.RunnerExecutable {
		return fmt.Errorf("%w: invalid managed runner migration executable", task.ErrEvidenceFault)
	}
	return nil
}

func matchManagedRunnerMigrationRecord(record managedRunnerMigrationRecord, td *TaskDir, meta *task.MetaRecord, specSHA256, metaSHA256 string) error {
	if record.RootID != td.store.RootID || record.TaskID != td.TaskID || record.SpecSHA256 != specSHA256 || record.MetaSHA256 != metaSHA256 || record.RunnerExecutable != meta.RunnerExecutable || meta.RunnerOwnership != "" {
		return task.ErrIdentityMismatch
	}
	return nil
}
