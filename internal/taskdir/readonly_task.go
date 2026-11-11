package taskdir

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// TaskEvidence contains validated local facts, never a live supervisor or OS
// observation. A missing task is represented by Exists=false. On error, callers
// may retain independently validated occurrence facts, not infer task absence.
type TaskEvidence struct {
	TaskID        string
	Exists        bool
	Inspection    *TaskInspection
	Submission    *task.SubmitRecord
	Receipt       *task.SupervisorReceipt
	Started       *task.ProviderStartedRecord
	Seal          *task.ProviderExitRecord
	Lifetime      task.LifetimeEvidence
	Stops         []StopRecord
	Logs          []LogDescriptor
	OutcomeSHA256 string
}

// ReadTaskEvidence uses the existing rooted validators without opening task
// locks or granting a TaskDir handle to the consumer. Records can advance while
// reading; missing dependencies are faults, never a new absence witness.
func (r *ReadOnlyStore) ReadTaskEvidence(ctx context.Context, id string) (TaskEvidence, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	evidence := TaskEvidence{TaskID: id}
	if err := r.checkReadContext(ctx); err != nil {
		return evidence, err
	}
	exists, err := r.store.TaskEntryExists(id)
	evidence.Exists = exists
	if err != nil || !exists {
		return evidence, err
	}
	td := &TaskDir{store: r.store, TaskID: id, Dir: filepath.Join(r.store.Root, "tasks", id)}
	// The lifetime owner records native worker Wait after the runner seals or
	// publishes normal completion. Read closure first, then publication: an
	// advancing writer must not pair new closure with an older no-seal view.
	evidence.Lifetime, err = LoadLifetimeEvidenceContext(ctx, td)
	if err != nil {
		return evidence, err
	}
	evidence.Inspection, err = td.InspectContext(ctx)
	if err != nil {
		return evidence, err
	}
	if err = readTaskEvidenceRecords(ctx, td, &evidence); err != nil {
		return evidence, err
	}
	evidence.Stops, err = td.ReadStopRecords()
	if err != nil {
		return evidence, err
	}
	evidence.Logs, err = td.LogDescriptorsContext(ctx)
	return evidence, errors.Join(err, ctx.Err())
}

func readTaskEvidenceRecords(ctx context.Context, td *TaskDir, evidence *TaskEvidence) error {
	var err error
	evidence.Submission, err = optionalTaskEvidenceRecord(ctx, td, "submit.json", td.ReadSubmission)
	if err != nil {
		return err
	}
	evidence.Receipt, err = optionalTaskEvidenceRecord(ctx, td, "supervisor.ref.json", td.ReadSupervisorReceipt)
	if err != nil {
		return err
	}
	evidence.Started, err = optionalTaskEvidenceRecord(ctx, td, "provider.started.json", td.readStartedEvidence)
	if err != nil {
		return err
	}
	if evidence.Inspection.SealExists {
		_, meta, specHash, metaHash, readErr := td.PreparedIdentity()
		if readErr != nil {
			return readErr
		}
		evidence.Seal, err = td.readSealContext(ctx, meta.Predicate, specHash, metaHash)
		if err != nil {
			return err
		}
	}
	if evidence.Inspection.Outcome != nil {
		evidence.OutcomeSHA256, err = td.OutcomeReferenceContext(ctx)
	}
	return errors.Join(err, ctx.Err())
}

type taskEvidenceRecord interface {
	*task.SubmitRecord | *task.SupervisorReceipt | *task.ProviderStartedRecord
}

func optionalTaskEvidenceRecord[T taskEvidenceRecord](ctx context.Context, td *TaskDir, name string, read func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	present, err := td.store.entryExists(filepath.Join(td.Dir, name))
	if err != nil || !present {
		return zero, err
	}
	record, err := read()
	if err != nil {
		return zero, errors.Join(task.ErrEvidenceFault, err)
	}
	return record, ctx.Err()
}

func (td *TaskDir) readStartedEvidence() (*task.ProviderStartedRecord, error) {
	_, _, specHash, metaHash, err := td.PreparedIdentity()
	if err != nil {
		return nil, err
	}
	var started task.ProviderStartedRecord
	if err = td.store.readRecord(filepath.Join(td.Dir, "provider.started.json"), &started); err != nil {
		return nil, err
	}
	if err = task.ValidateProviderStartedRecord(&started); err != nil {
		return nil, err
	}
	if started.RootID != td.store.RootID || started.TaskID != td.TaskID || started.SpecSHA256 != specHash || started.MetaSHA256 != metaHash {
		return nil, task.ErrIdentityMismatch
	}
	present, err := td.store.exists(filepath.Join(td.Dir, "provider.start"))
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, task.ErrEvidenceFault
	}
	return &started, nil
}
