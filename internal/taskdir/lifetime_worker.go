package taskdir

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// LifetimeWorkerDigest returns the digest of the canonical persisted worker
// winner using taskdir's persisted-byte identity semantics.
func (td *TaskDir) LifetimeWorkerDigest() (string, error) {
	return td.LifetimeWorkerDigestContext(context.Background())
}

// LifetimeWorkerDigestContext validates and hashes the persisted worker winner
// using the caller's context for both reads.
func (td *TaskDir) LifetimeWorkerDigestContext(ctx context.Context) (string, error) {
	if _, err := td.ReadLifetimeWorkerContext(ctx); err != nil {
		return "", err
	}
	data, err := td.store.readBytesContext(ctx, filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerName), task.MaxControlRecordSize)
	if err != nil {
		return "", lifetimeValidationDependencyError(err)
	}
	return task.ComputeSHA256(data), nil
}

func (td *TaskDir) RecordLifetimeWorker(facts LifetimeWorkerFacts) (*task.LifetimeWorkerRecord, error) {
	return td.RecordLifetimeWorkerContext(context.Background(), facts)
}

// RecordLifetimeWorkerContext persists the managed worker identity under the
// caller's bounded persistence context.
func (td *TaskDir) RecordLifetimeWorkerContext(ctx context.Context, facts LifetimeWorkerFacts) (*task.LifetimeWorkerRecord, error) {
	ctx, cancel, err := persistenceContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if err = td.rejectRecoveredLifetimeLaunchContext(ctx); err != nil {
		return nil, err
	}

	registration, err := td.ReadLifetimeRegistrationContext(ctx)
	if err != nil {
		return nil, err
	}
	registrationDigest, err := td.LifetimeRegistrationDigestContext(ctx)
	if err != nil {
		return nil, err
	}
	record := task.LifetimeWorkerRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             td.store.RootID,
		TaskID:             td.TaskID,
		RegistrationSHA256: registrationDigest,
		PID:                facts.PID,
		ProcessStart:       facts.ProcessStart,
		BootID:             facts.BootID,
		StartedAt:          timestamp(),
	}
	if err = task.ValidateLifetimeWorkerRecord(&record); err != nil {
		return nil, err
	}
	if record.BootID != registration.BootID {
		return nil, task.ErrIdentityMismatch
	}
	err = td.recordSameContext(ctx, filepath.Join(lifetimeDirectory, lifetimeWorkerName), record, func() (bool, error) {
		existing, readErr := td.ReadLifetimeWorkerContext(ctx)
		if readErr != nil {
			return false, readErr
		}
		record.StartedAt = existing.StartedAt
		return reflect.DeepEqual(existing, &record), nil
	})
	if err != nil {
		return nil, err
	}
	return td.ReadLifetimeWorkerContext(ctx)
}

func (td *TaskDir) ReadLifetimeWorker() (*task.LifetimeWorkerRecord, error) {
	return td.ReadLifetimeWorkerContext(context.Background())
}

// ReadLifetimeWorkerContext validates the worker and its registration binding
// without detaching any nested read from the caller's context.
func (td *TaskDir) ReadLifetimeWorkerContext(ctx context.Context) (*task.LifetimeWorkerRecord, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	var record task.LifetimeWorkerRecord
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerName)
	if err := td.store.readRecordContext(ctx, path, &record); err != nil {
		return nil, err
	}
	if err := task.ValidateLifetimeWorkerRecord(&record); err != nil {
		return nil, err
	}
	registration, err := td.ReadLifetimeRegistrationContext(ctx)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	registrationDigest, err := td.LifetimeRegistrationDigestContext(ctx)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	if record.RootID != td.store.RootID || record.TaskID != td.TaskID || record.RegistrationSHA256 != registrationDigest || record.BootID != registration.BootID {
		return nil, task.ErrIdentityMismatch
	}
	return &record, nil
}

// RecordLifetimeWorkerWait persists the create-once managed-worker completion
// disposition. An empty WorkerSHA256 is accepted when no worker started or
// when an exact scope has no persisted worker identity to bind to.
func (td *TaskDir) RecordLifetimeWorkerWait(facts LifetimeWorkerWaitFacts) (*task.LifetimeWorkerWaitRecord, error) {
	return td.RecordLifetimeWorkerWaitContext(context.Background(), facts)
}

// RecordLifetimeWorkerWaitContext persists the managed-worker completion
// disposition under the caller's bounded persistence context.
func (td *TaskDir) RecordLifetimeWorkerWaitContext(ctx context.Context, facts LifetimeWorkerWaitFacts) (*task.LifetimeWorkerWaitRecord, error) {
	ctx, cancel, err := persistenceContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	registration, registrationDigest, worker, workerDigest, err := td.lifetimeWorkerWaitBindingContext(ctx)
	if err != nil {
		return nil, err
	}
	observedAt := facts.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	record := task.LifetimeWorkerWaitRecord{
		SchemaVersion:      task.SchemaVersion,
		RootID:             td.store.RootID,
		TaskID:             td.TaskID,
		RegistrationSHA256: registrationDigest,
		WorkerSHA256:       facts.WorkerSHA256,
		ChildPID:           facts.ChildPID,
		Result:             facts.Result,
		ExitCode:           facts.ExitCode,
		ObservedAt:         observedAt.UTC().Format(time.RFC3339Nano),
	}
	if err = validateLifetimeWorkerWaitForTask(&record, registration, registrationDigest, worker, workerDigest); err != nil {
		return nil, err
	}
	return td.recordLifetimeWorkerWaitCandidateContext(ctx, record)
}

func (td *TaskDir) recordLifetimeWorkerWaitCandidateContext(ctx context.Context, record task.LifetimeWorkerWaitRecord) (*task.LifetimeWorkerWaitRecord, error) {
	err := td.recordSameContext(ctx, filepath.Join(lifetimeDirectory, lifetimeWorkerWaitName), record, func() (bool, error) {
		existing, readErr := td.ReadLifetimeWorkerWaitContext(ctx)
		if readErr != nil {
			return false, readErr
		}
		record.ObservedAt = existing.ObservedAt
		return reflect.DeepEqual(existing, &record), nil
	})
	if err != nil {
		winner, readErr := td.ReadLifetimeWorkerWaitContext(ctx)
		if readErr != nil {
			return nil, errors.Join(err, readErr)
		}
		record.ObservedAt = winner.ObservedAt
		if !reflect.DeepEqual(winner, &record) {
			return nil, errors.Join(err, task.ErrIdentityMismatch)
		}
		return winner, err
	}
	return td.ReadLifetimeWorkerWaitContext(ctx)
}

// ReadLifetimeWorkerWait returns the strictly validated child Wait winner.
func (td *TaskDir) ReadLifetimeWorkerWait() (*task.LifetimeWorkerWaitRecord, error) {
	return td.ReadLifetimeWorkerWaitContext(context.Background())
}

// ReadLifetimeWorkerWaitContext validates the child Wait winner and all of its
// registration and worker evidence within the caller's context.
func (td *TaskDir) ReadLifetimeWorkerWaitContext(ctx context.Context) (*task.LifetimeWorkerWaitRecord, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return nil, err
	}
	var record task.LifetimeWorkerWaitRecord
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeWorkerWaitName)
	if err := td.store.readRecordContext(ctx, path, &record); err != nil {
		return nil, err
	}
	if err := task.ValidateLifetimeWorkerWaitRecord(&record); err != nil {
		return nil, err
	}
	if record.Result == task.LifetimeWorkerWaitAbsent {
		evidence, err := loadLifetimeDependenciesContext(ctx, td)
		if err != nil {
			return nil, lifetimeCompletionDependencyError(err)
		}
		if err = task.ValidateLifetimeCompletionBinding(&record, evidence); err != nil {
			return nil, err
		}
		return &record, nil
	}
	registration, registrationDigest, worker, workerDigest, err := td.lifetimeWorkerWaitBindingContext(ctx)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	if err := validateLifetimeWorkerWaitForTask(&record, registration, registrationDigest, worker, workerDigest); err != nil {
		return nil, err
	}
	return &record, nil
}

func (td *TaskDir) lifetimeWorkerWaitBindingContext(ctx context.Context) (*task.LifetimeRegistrationRecord, string, *task.LifetimeWorkerRecord, string, error) {
	registration, err := td.ReadLifetimeRegistrationContext(ctx)
	if err != nil {
		return nil, "", nil, "", err
	}
	registrationDigest, err := td.LifetimeRegistrationDigestContext(ctx)
	if err != nil {
		return nil, "", nil, "", err
	}
	worker, workerErr := td.ReadLifetimeWorkerContext(ctx)
	if task.RecordAbsent(workerErr) {
		return registration, registrationDigest, nil, "", nil
	}
	if workerErr != nil {
		return nil, "", nil, "", workerErr
	}
	workerDigest, err := td.LifetimeWorkerDigestContext(ctx)
	if err != nil {
		return nil, "", nil, "", err
	}
	return registration, registrationDigest, worker, workerDigest, nil
}

func validateLifetimeWorkerWaitForTask(record *task.LifetimeWorkerWaitRecord, registration *task.LifetimeRegistrationRecord, registrationDigest string, worker *task.LifetimeWorkerRecord, workerDigest string) error {
	if !task.LifetimeWorkerWaitMatchesRegistration(record, registration, registrationDigest) {
		return task.ErrIdentityMismatch
	}
	if worker != nil && !task.LifetimeWorkerWaitMatchesWorker(record, worker, workerDigest) {
		return task.ErrIdentityMismatch
	}
	return task.ValidateLifetimeCompletionBinding(record, task.LifetimeEvidence{Registration: registration, Worker: worker})
}
