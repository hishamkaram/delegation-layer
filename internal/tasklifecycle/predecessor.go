package tasklifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func preparePredecessorResolution(store *taskdir.Store, td *taskdir.TaskDir, predecessorReq *task.TaskRecord, predecessorMeta *task.MetaRecord, options supervisorcore.AdmissionOptions) (timedOut, submitted bool, resultErr error) {
	inspection, err := td.Inspect()
	if err != nil {
		return false, false, err
	}
	if supervisorcore.TaskTerminal(inspection) {
		return false, true, nil
	}
	timedOut, err = releaseTimedOutPredecessor(td, predecessorReq)
	if err != nil || timedOut {
		return timedOut, false, err
	}
	if _, submitErr := td.ReadSubmission(); submitErr == nil {
		if repairErr := supervisorcore.RepairQueueForTask(context.Background(), store, predecessorMeta.SupervisorConfig, predecessorMeta, options); repairErr != nil {
			return false, false, repairErr
		}
		return false, true, nil
	} else if !errors.Is(submitErr, os.ErrNotExist) {
		return false, false, submitErr
	}
	return false, false, nil
}

func ResolvePredecessor(store *taskdir.Store, options PredecessorOptions, request task.TaskRecord, id string) (prior *task.PriorSession, resultErr error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	td, err := store.OpenTask(id)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	predecessorReq, predecessorMeta, err := td.PreparedRecords()
	if err != nil {
		return nil, err
	}
	prior, err = ContinuationPrior(td, predecessorReq)
	if err != nil {
		return nil, err
	}
	if _, admissionErr := admissioncore.ValidateContinuation(options.Admission, options.ApplyEnvironment, store.Root, *predecessorReq, *predecessorMeta, prior, request); admissionErr != nil {
		return nil, admissionErr
	}
	timedOut, submitted, err := preparePredecessorResolution(store, td, predecessorReq, predecessorMeta, options.Supervisor)
	if err != nil {
		return nil, err
	}
	if timedOut {
		return prior, nil
	}
	if !submitted {
		return nil, task.ErrRequestConflict
	}
	if err = PredecessorTerminated(store.Root, td, predecessorReq, SupervisorOptions{Options: options.Supervisor.Control, InitialExecutable: options.Supervisor.InitialExecutable, OpenStore: options.Supervisor.OpenStore}); err != nil {
		return nil, err
	}
	return prior, nil
}

func ContinuationPrior(td *taskdir.TaskDir, predecessor *task.TaskRecord) (*task.PriorSession, error) {
	if predecessor == nil {
		return nil, task.ErrInvariantFault
	}
	identity, err := td.ReadProviderIdentity()
	if err != nil {
		return nil, fmt.Errorf("predecessor session is unavailable: %w", err)
	}
	return &task.PriorSession{Provider: identity.Provider, ConversationID: identity.ConversationID, PredecessorTaskID: predecessor.TaskID}, nil
}
