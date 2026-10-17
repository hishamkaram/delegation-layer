package tasklifecycle

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func RecoverStagedSubmission(ctx context.Context, root string, td *taskdir.TaskDir, client *pueue.Client, observation pueue.Observation) (pueue.Observation, error) {
	if pueue.IsPrivateConfig(root, client.Binding().ConfigPath) {
		controlContext, cancel := context.WithTimeout(ctx, pueue.PrivateStatusObservationTimeout)
		repairErr := supervisorcore.EnsureAdmissionSchedulingContext(controlContext, root, client)
		cancel()
		if repairErr != nil {
			return observation, repairErr
		}
	}
	commit := func(current pueue.Observation) error {
		receipt, err := current.Receipt()
		if err != nil {
			return err
		}
		return td.RecordSupervisorReceiptContext(ctx, *receipt)
	}
	recovered, err := client.RecoverStagedWithGate(ctx, observation.Identity, observation.NumericTaskID, supervisorcore.SubmissionGate(root), commit)
	if err == nil || !errors.Is(err, pueue.ErrUnknown) {
		return recovered, err
	}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr != nil || receipt.NumericTaskID != observation.NumericTaskID {
		return recovered, errors.Join(err, receiptErr)
	}
	current, observeErr := client.ReconcileStatus(ctx, observation.Identity)
	if observeErr != nil || !current.Matched || current.NumericTaskID != observation.NumericTaskID {
		return current, errors.Join(err, observeErr)
	}
	switch current.State {
	case pueue.StateQueued, pueue.StateRunning, pueue.StateEnded:
		return current, RequireDurableSupervisorReceipt(td, current)
	case pueue.StateUnknown, pueue.StateStashed:
		return current, err
	}
	return current, err
}

func RequireDurableSupervisorReceipt(td *taskdir.TaskDir, observation pueue.Observation) error {
	if td == nil || !observation.Matched {
		return task.ErrEvidenceFault
	}
	observed, err := observation.Receipt()
	if err != nil {
		return err
	}
	recorded, err := td.ReadSupervisorReceipt()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.Join(task.ErrEvidenceFault, err)
		}
		return err
	}
	if *recorded != *observed {
		return task.ErrIdentityMismatch
	}
	return nil
}
