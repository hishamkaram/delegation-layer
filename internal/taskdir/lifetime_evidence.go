package taskdir

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// LifetimeRecordReader supplies validated records. Closure policy belongs to
// task.ResolveLifetimeClosure, not to individual storage or operation families.
type LifetimeRecordReader interface {
	ReadLifetimeIntentContext(context.Context) (*task.LifetimeIntentRecord, error)
	ReadLifetimeRegistrationContext(context.Context) (*task.LifetimeRegistrationRecord, error)
	ReadLifetimeWorkerContext(context.Context) (*task.LifetimeWorkerRecord, error)
	ReadLifetimeWorkerWaitContext(context.Context) (*task.LifetimeWorkerWaitRecord, error)
	ReadLifetimeLaunchesContext(context.Context) ([]task.LifetimeLaunchRecord, error)
	ReadLifetimeTerminationContext(context.Context) (*task.LifetimeTerminationRecord, error)
}

// LoadLifetimeEvidenceContext reads the complete durable evidence without
// inferring process absence or performing recovery. Transition callers retain
// their existing lifetime-owner lease around this read and any following write.
func LoadLifetimeEvidenceContext(ctx context.Context, records LifetimeRecordReader) (task.LifetimeEvidence, error) {
	if ctx == nil {
		return task.LifetimeEvidence{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return task.LifetimeEvidence{}, err
	}
	completion, err := optionalLifetimeEvidenceRecord(records.ReadLifetimeWorkerWaitContext(ctx))
	if err != nil {
		return task.LifetimeEvidence{}, err
	}
	evidence, err := loadLifetimeDependenciesContext(ctx, records)
	if err != nil {
		return evidence, err
	}
	evidence.WorkerWait = completion
	if _, err = task.ResolveLifetimeClosure(evidence); err != nil {
		return evidence, err
	}
	return evidence, nil
}

func loadLifetimeDependenciesContext(ctx context.Context, records LifetimeRecordReader) (task.LifetimeEvidence, error) {
	var evidence task.LifetimeEvidence
	if ctx == nil {
		return evidence, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return evidence, err
	}
	var err error
	// Read dependents before their immutable prerequisites. The transition owner
	// fences launches before termination and registration after absence. A read
	// may miss a newer completion, but cannot combine it with older dependencies.
	evidence.Termination, err = optionalLifetimeEvidenceRecord(records.ReadLifetimeTerminationContext(ctx))
	if err != nil {
		return evidence, err
	}
	evidence.Launches, err = records.ReadLifetimeLaunchesContext(ctx)
	if task.RecordAbsent(err) {
		err = nil
	}
	if err != nil {
		return evidence, err
	}
	evidence.Worker, err = optionalLifetimeEvidenceRecord(records.ReadLifetimeWorkerContext(ctx))
	if err != nil {
		return evidence, err
	}
	evidence.Registration, err = optionalLifetimeEvidenceRecord(records.ReadLifetimeRegistrationContext(ctx))
	if err != nil {
		return evidence, err
	}
	evidence.Intent, err = optionalLifetimeEvidenceRecord(records.ReadLifetimeIntentContext(ctx))
	return evidence, err
}

type lifetimeEvidenceRecord interface {
	*task.LifetimeIntentRecord | *task.LifetimeRegistrationRecord | *task.LifetimeWorkerRecord |
		*task.LifetimeWorkerWaitRecord | *task.LifetimeTerminationRecord
}

func optionalLifetimeEvidenceRecord[T lifetimeEvidenceRecord](record T, err error) (T, error) {
	if task.RecordAbsent(err) {
		return nil, nil
	}
	return record, err
}

// RecordLifetimeWorkerAbsenceContext is called only by the lifetime transition
// owner after a positive native absence witness. The original completion
// destination remains create-once, including an ambiguous publication retry.
func (td *TaskDir) RecordLifetimeWorkerAbsenceContext(ctx context.Context, proof task.LifetimeAbsenceProof, observedBootID string) (*task.LifetimeWorkerWaitRecord, error) {
	ctx, cancel, err := persistenceContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	evidence, err := loadLifetimeDependenciesContext(ctx, td)
	if err != nil {
		return nil, err
	}
	candidate, err := task.NewLifetimeWorkerAbsence(evidence, proof, observedBootID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return td.recordLifetimeWorkerWaitCandidateContext(ctx, candidate)
}

func (l *ControlLifetime) RecordLifetimeWorkerAbsenceContext(ctx context.Context, proof task.LifetimeAbsenceProof, observedBootID string) (result *task.LifetimeWorkerWaitRecord, resultErr error) {
	if err := l.validateLifetimeOperation(true); err != nil {
		return nil, err
	}
	err := withControlLifetimeTransactionContext(ctx, l.control, func(tx *ControlTransaction) error {
		evidence, readErr := loadLifetimeDependenciesContext(ctx, controlLifetimeTransactionReader{tx: tx, lifetime: l})
		if readErr != nil {
			return readErr
		}
		candidate, buildErr := task.NewLifetimeWorkerAbsence(evidence, proof, observedBootID, time.Now().UTC())
		if buildErr != nil {
			return buildErr
		}
		result, readErr = recordControlLifetimeWorkerWaitTx(tx, l, candidate)
		return readErr
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

// A transaction reader reuses the same dependency loader without reacquiring
// the control lock. It supplies storage reads only, never closure decisions.
type controlLifetimeTransactionReader struct {
	tx       *ControlTransaction
	lifetime *ControlLifetime
}

func (r controlLifetimeTransactionReader) ReadLifetimeIntentContext(context.Context) (*task.LifetimeIntentRecord, error) {
	return readControlLifetimeIntentTx(r.tx, r.lifetime)
}

func (r controlLifetimeTransactionReader) ReadLifetimeRegistrationContext(context.Context) (*task.LifetimeRegistrationRecord, error) {
	record, _, err := readControlLifetimeRegistrationTx(r.tx, r.lifetime)
	return record, err
}

func (r controlLifetimeTransactionReader) ReadLifetimeWorkerContext(context.Context) (*task.LifetimeWorkerRecord, error) {
	return readControlLifetimeWorkerTx(r.tx, r.lifetime)
}

func (r controlLifetimeTransactionReader) ReadLifetimeWorkerWaitContext(context.Context) (*task.LifetimeWorkerWaitRecord, error) {
	return readControlLifetimeWorkerWaitTx(r.tx, r.lifetime)
}

func (r controlLifetimeTransactionReader) ReadLifetimeLaunchesContext(context.Context) ([]task.LifetimeLaunchRecord, error) {
	_, digest, err := readControlLifetimeRegistrationTx(r.tx, r.lifetime)
	if err != nil && !task.RecordAbsent(err) {
		return nil, err
	}
	// An orphan launch must be seen and rejected even without a registration.
	return readControlLifetimeLaunchesTx(r.tx, r.lifetime, digest)
}

func (r controlLifetimeTransactionReader) ReadLifetimeTerminationContext(context.Context) (*task.LifetimeTerminationRecord, error) {
	return readBoundControlLifetimeTerminationTx(r.tx, r.lifetime)
}

var _ LifetimeRecordReader = controlLifetimeTransactionReader{}

// Missing completion dependencies are faults, never a new absence witness.
func lifetimeCompletionDependencyError(err error) error {
	return errors.Join(task.ErrEvidenceFault, lifetimeValidationDependencyError(err))
}

func (td *TaskDir) rejectRecoveredLifetimeLaunchContext(ctx context.Context) error {
	completion, err := td.ReadLifetimeWorkerWaitContext(ctx)
	if task.RecordAbsent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if task.LifetimeWorkerWaitProvesAbsence(completion) {
		return task.ErrTerminalTask
	}
	return nil
}
