package admission

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func acceptInspectionObservation(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, identity *pueue.InspectionIdentity, job pueue.Job) (json.RawMessage, bool, error) {
	if err := RequireInspectionReceipt(ctx, operation, job.ID); err != nil {
		return nil, false, err
	}
	id := job.ID
	identity.NumericTaskID = &id
	if facts, _, terminal, err := RecordedInspectionFacts(ctx, operation); terminal || err != nil {
		return facts, terminal, err
	}
	if job.State != pueue.StateEnded {
		return nil, false, nil
	}
	if operation.Request().Binding.WorkerExitProof == inspection.WorkerExitProofGuardianWaitV1 {
		// Pueue reports the guardian process, not the exact inspection child.
		// Managed exit authority is published only by the guardian after Wait.
		return nil, false, nil
	}
	if !job.Succeeded {
		if err := operation.RecordWorkerExitContext(ctx, job.ID, false); errors.Is(err, os.ErrNotExist) {
			return nil, true, ErrInspectionWorkerUnavailable
		} else if err != nil {
			return nil, true, err
		}
		return nil, true, ErrInspectionWorkerUnavailable
	}
	recordErr := operation.RecordWorkerExitContext(ctx, job.ID, job.Succeeded)
	if errors.Is(recordErr, os.ErrNotExist) {
		// Pueue can report the row just before the worker's final control record
		// becomes observable. Preserve the row and retry the same observation.
		return nil, false, nil
	} else if recordErr != nil {
		return nil, true, recordErr
	}
	facts, _, terminal, err := RecordedInspectionFacts(ctx, operation)
	return facts, terminal, err
}

// requireInspectionReceipt keeps the staged submission commit as the sole
// creator of inspection admission authority. Later queue observations only
// validate the exact numeric task they were already authorized to observe.
func RequireInspectionReceipt(ctx context.Context, operation *inspection.Operation, numericTaskID int64) error {
	if operation == nil || numericTaskID < 0 {
		return task.ErrEvidenceFault
	}
	receipt, err := operation.ReceiptContext(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.Join(task.ErrEvidenceFault, err)
		}
		return err
	}
	if receipt.NumericTaskID == nil || *receipt.NumericTaskID != numericTaskID {
		return task.ErrIdentityMismatch
	}
	return nil
}

func reconcileInspectionObservation(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, identity pueue.InspectionIdentity, commit pueue.InspectionObservationCommit) (pueue.InspectionObservation, error) {
	if inspectionDeadlineReached(ctx, operation) {
		return pueue.InspectionObservation{}, inspection.ErrAdmissionExpired
	}
	committed := commit
	if commit != nil {
		committed = func(observation pueue.InspectionObservation) error {
			if observation.Job.State == pueue.StateStashed {
				return nil
			}
			return commit(observation)
		}
	}
	observation, observeErr := supervisor.ReconcileInspectionWithLease(ctx, identity, committed)
	if inspectionDeadlineReached(ctx, operation) {
		// The supervisor observation may reach the deadline with either an
		// in-flight status command or a terminal failed worker. The persisted
		// admission deadline is authoritative in both cases. Preserve a pending
		// command in the error so its supervisor lease outlives this caller.
		return observation, errors.Join(inspection.ErrAdmissionExpired, observeErr)
	}
	if observeErr == nil && observation.Matched && observation.Job.State == pueue.StateStashed {
		return recoverStagedInspectionObservation(ctx, operation, supervisor, observation, commit)
	}
	return observation, observeErr
}

func recoverStagedInspectionObservation(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, observation pueue.InspectionObservation, commit pueue.InspectionObservationCommit) (pueue.InspectionObservation, error) {
	receiptCommit := func(current pueue.InspectionObservation) error {
		return operation.RecordReceiptContext(ctx, current.Job.ID)
	}
	root := filepath.Dir(filepath.Dir(observation.Binding.ConfigPath))
	receiptMatches := func(id int64) error {
		receipt, receiptErr := operation.ReceiptContext(ctx)
		if receiptErr != nil {
			return receiptErr
		}
		if receipt.NumericTaskID == nil || *receipt.NumericTaskID != id {
			return task.ErrIdentityMismatch
		}
		return nil
	}
	return supervisorcore.RecoverStagedAuxiliaryObservation(ctx, supervisor, root, observation, receiptCommit, receiptMatches, commit)
}

func CompletedInspectionFacts(ctx context.Context, operation *inspection.Operation, job pueue.Job) (json.RawMessage, error) {
	if operation == nil {
		if !job.Succeeded {
			return nil, ErrInspectionWorkerUnavailable
		}
		return nil, task.ErrEvidenceFault
	}
	if !job.Succeeded {
		return nil, ErrInspectionWorkerUnavailable
	}
	if err := operation.RecordWorkerExitContext(ctx, job.ID, true); err != nil {
		return nil, err
	}
	facts, _, terminal, err := RecordedInspectionFacts(ctx, operation)
	if err != nil || terminal {
		return facts, err
	}
	return nil, os.ErrNotExist
}
