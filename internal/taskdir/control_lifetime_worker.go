package taskdir

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// RecordLifetimeWorker persists the managed worker identity for the current
// canonical registration.
func (l *ControlLifetime) RecordLifetimeWorker(facts LifetimeWorkerFacts) (*task.LifetimeWorkerRecord, error) {
	return l.RecordLifetimeWorkerContext(context.Background(), facts)
}

// RecordLifetimeWorkerContext persists the worker identity under a bounded
// control transaction.
func (l *ControlLifetime) RecordLifetimeWorkerContext(ctx context.Context, facts LifetimeWorkerFacts) (result *task.LifetimeWorkerRecord, resultErr error) {
	if err := l.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		if err := rejectLifetimeLaunchPermissionTx(tx, l); err != nil {
			return err
		}

		registration, registrationDigest, readErr := readControlLifetimeRegistrationTx(tx, l)
		if readErr != nil {
			return readErr
		}
		candidate := task.LifetimeWorkerRecord{
			SchemaVersion:      task.SchemaVersion,
			RootID:             l.rootID,
			TaskID:             l.operationID,
			RegistrationSHA256: registrationDigest,
			PID:                facts.PID,
			ProcessStart:       facts.ProcessStart,
			BootID:             facts.BootID,
			StartedAt:          timestamp(),
		}
		if err := validateControlLifetimeWorker(l, candidate, registration, registrationDigest); err != nil {
			return err
		}
		result, readErr = recordControlLifetimeWorkerTx(tx, l, candidate)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordControlLifetimeWorkerTx(tx *ControlTransaction, l *ControlLifetime, candidate task.LifetimeWorkerRecord) (*task.LifetimeWorkerRecord, error) {
	existing, readErr := readControlLifetimeWorkerTx(tx, l)
	if readErr == nil {
		candidate.StartedAt = existing.StartedAt
		if !reflect.DeepEqual(*existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, nil
	}
	if !task.RecordAbsent(readErr) {
		return nil, readErr
	}
	created, putErr := tx.Put(controlLifetimeWorkerName, candidate)
	if putErr != nil {
		return nil, putErr
	}
	if created {
		return &candidate, nil
	}
	winner, winnerErr := readControlLifetimeWorkerTx(tx, l)
	if winnerErr != nil {
		return nil, winnerErr
	}
	candidate.StartedAt = winner.StartedAt
	if !reflect.DeepEqual(*winner, candidate) {
		return nil, task.ErrIdentityMismatch
	}
	return winner, nil
}

// ReadLifetimeWorker returns the worker identity bound to the current
// registration.
func (l *ControlLifetime) ReadLifetimeWorker() (*task.LifetimeWorkerRecord, error) {
	return l.ReadLifetimeWorkerContext(context.Background())
}

// ReadLifetimeWorkerContext reads the worker identity under a bounded
// transaction.
func (l *ControlLifetime) ReadLifetimeWorkerContext(ctx context.Context) (result *task.LifetimeWorkerRecord, resultErr error) {
	if err := l.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var readErr error
		result, readErr = readControlLifetimeWorkerTx(tx, l)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RecordLifetimeWorkerWait persists the create-once worker completion
// disposition.
func (l *ControlLifetime) RecordLifetimeWorkerWait(facts LifetimeWorkerWaitFacts) (*task.LifetimeWorkerWaitRecord, error) {
	return l.RecordLifetimeWorkerWaitContext(context.Background(), facts)
}

// RecordLifetimeWorkerWaitContext persists a worker completion disposition
// while honoring the caller's bounded lifetime.
func (l *ControlLifetime) RecordLifetimeWorkerWaitContext(ctx context.Context, facts LifetimeWorkerWaitFacts) (result *task.LifetimeWorkerWaitRecord, resultErr error) {
	if err := l.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	observedAt := facts.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		registration, registrationDigest, readErr := readControlLifetimeRegistrationTx(tx, l)
		if readErr != nil {
			return readErr
		}
		worker, workerDigest, workerErr := readControlLifetimeWorkerBindingTx(tx, l)
		if workerErr != nil {
			return workerErr
		}
		candidate := task.LifetimeWorkerWaitRecord{
			SchemaVersion:      task.SchemaVersion,
			RootID:             l.rootID,
			TaskID:             l.operationID,
			RegistrationSHA256: registrationDigest,
			WorkerSHA256:       facts.WorkerSHA256,
			ChildPID:           facts.ChildPID,
			Result:             facts.Result,
			ExitCode:           facts.ExitCode,
			ObservedAt:         observedAt.UTC().Format(time.RFC3339Nano),
		}
		if err := validateControlLifetimeWorkerWait(l, candidate, registration, registrationDigest, worker, workerDigest); err != nil {
			return err
		}
		result, readErr = recordControlLifetimeWorkerWaitTx(tx, l, candidate)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordControlLifetimeWorkerWaitTx(tx *ControlTransaction, l *ControlLifetime, candidate task.LifetimeWorkerWaitRecord) (*task.LifetimeWorkerWaitRecord, error) {
	existing, readErr := readControlLifetimeWorkerWaitTx(tx, l)
	if readErr == nil {
		candidate.ObservedAt = existing.ObservedAt
		if !reflect.DeepEqual(*existing, candidate) {
			return nil, task.ErrIdentityMismatch
		}
		return existing, nil
	}
	if !task.RecordAbsent(readErr) {
		return nil, readErr
	}
	created, putErr := tx.Put(controlLifetimeWorkerWaitName, candidate)
	if created && putErr == nil {
		return &candidate, nil
	}
	winner, winnerErr := readControlLifetimeWorkerWaitTx(tx, l)
	if winnerErr != nil {
		return nil, errors.Join(putErr, winnerErr)
	}
	candidate.ObservedAt = winner.ObservedAt
	if !reflect.DeepEqual(*winner, candidate) {
		return nil, errors.Join(putErr, task.ErrIdentityMismatch)
	}
	return winner, putErr
}

// ReadLifetimeWorkerWait returns the strictly validated child Wait winner.
func (l *ControlLifetime) ReadLifetimeWorkerWait() (*task.LifetimeWorkerWaitRecord, error) {
	return l.ReadLifetimeWorkerWaitContext(context.Background())
}

// ReadLifetimeWorkerWaitContext reads the child Wait winner under a bounded
// control transaction.
func (l *ControlLifetime) ReadLifetimeWorkerWaitContext(ctx context.Context) (result *task.LifetimeWorkerWaitRecord, resultErr error) {
	if err := l.validateLifetimeOperation(false); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		var readErr error
		result, readErr = readControlLifetimeWorkerWaitTx(tx, l)
		return readErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func validateControlLifetimeWorker(l *ControlLifetime, record task.LifetimeWorkerRecord, registration *task.LifetimeRegistrationRecord, registrationDigest string) error {
	if err := task.ValidateLifetimeWorkerRecord(&record); err != nil {
		return err
	}
	if record.RootID != l.rootID || record.TaskID != l.operationID || record.RegistrationSHA256 != registrationDigest || registration == nil || record.BootID != registration.BootID {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateControlLifetimeWorkerWait(l *ControlLifetime, record task.LifetimeWorkerWaitRecord, registration *task.LifetimeRegistrationRecord, registrationDigest string, worker *task.LifetimeWorkerRecord, workerDigest string) error {
	if record.RootID != l.rootID || record.TaskID != l.operationID {
		return task.ErrIdentityMismatch
	}
	return validateLifetimeWorkerWaitForTask(&record, registration, registrationDigest, worker, workerDigest)
}

func readControlLifetimeWorkerTx(tx *ControlTransaction, l *ControlLifetime) (*task.LifetimeWorkerRecord, error) {
	var record task.LifetimeWorkerRecord
	if err := tx.Read(controlLifetimeWorkerName, &record); err != nil {
		return nil, controlLifetimeReadError(controlLifetimeWorkerName, err)
	}
	if err := task.ValidateLifetimeWorkerRecord(&record); err != nil {
		return nil, err
	}
	registration, registrationDigest, err := readControlLifetimeRegistrationTx(tx, l)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	if err = validateControlLifetimeWorker(l, record, registration, registrationDigest); err != nil {
		return nil, err
	}
	if err = tx.Verify(controlLifetimeWorkerName, record); err != nil {
		return nil, err
	}
	return &record, nil
}

func readControlLifetimeWorkerBindingTx(tx *ControlTransaction, l *ControlLifetime) (*task.LifetimeWorkerRecord, string, error) {
	worker, err := readControlLifetimeWorkerTx(tx, l)
	if task.RecordAbsent(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	digest, err := controlLifetimeRecordDigest(worker)
	if err != nil {
		return nil, "", err
	}
	return worker, digest, nil
}

func readControlLifetimeWorkerWaitTx(tx *ControlTransaction, l *ControlLifetime) (*task.LifetimeWorkerWaitRecord, error) {
	var record task.LifetimeWorkerWaitRecord
	if err := tx.Read(controlLifetimeWorkerWaitName, &record); err != nil {
		return nil, controlLifetimeReadError(controlLifetimeWorkerWaitName, err)
	}
	if err := task.ValidateLifetimeWorkerWaitRecord(&record); err != nil {
		return nil, err
	}
	if record.Result == task.LifetimeWorkerWaitAbsent {
		evidence, err := loadLifetimeDependenciesContext(tx.ctx, controlLifetimeTransactionReader{tx: tx, lifetime: l})
		if err != nil {
			return nil, lifetimeCompletionDependencyError(err)
		}
		if record.RootID != l.rootID || record.TaskID != l.operationID {
			return nil, task.ErrIdentityMismatch
		}
		if err = task.ValidateLifetimeCompletionBinding(&record, evidence); err != nil {
			return nil, err
		}
		if err = tx.Verify(controlLifetimeWorkerWaitName, record); err != nil {
			return nil, err
		}
		return &record, nil
	}
	registration, registrationDigest, err := readControlLifetimeRegistrationTx(tx, l)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	worker, workerDigest, err := readControlLifetimeWorkerBindingTx(tx, l)
	if err != nil {
		return nil, lifetimeValidationDependencyError(err)
	}
	if err = validateControlLifetimeWorkerWait(l, record, registration, registrationDigest, worker, workerDigest); err != nil {
		return nil, err
	}
	if err = tx.Verify(controlLifetimeWorkerWaitName, record); err != nil {
		return nil, err
	}
	return &record, nil
}
