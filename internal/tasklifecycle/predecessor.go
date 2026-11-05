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

func preparePredecessorResolution(ctx context.Context, store *taskdir.Store, td *taskdir.TaskDir, predecessorReq *task.TaskRecord, predecessorMeta *task.MetaRecord, options supervisorcore.AdmissionOptions) (handedOff, submitted bool, resultErr error) {
	inspection, err := td.Inspect()
	if err != nil {
		return false, false, err
	}
	if supervisorcore.TaskTerminal(inspection) {
		return false, true, nil
	}
	handedOff, err = releaseHandedOffPredecessor(ctx, td, predecessorReq)
	if err != nil || handedOff {
		return handedOff, false, err
	}
	if submission, submitErr := td.ReadSubmission(); submitErr == nil {
		if repairErr := supervisorcore.RepairQueueForSubmittedTask(ctx, store, submission, predecessorMeta, options); repairErr != nil {
			return false, false, repairErr
		}
		return false, true, nil
	} else if !errors.Is(submitErr, os.ErrNotExist) {
		return false, false, submitErr
	}
	return false, false, nil
}

func ResolvePredecessor(store *taskdir.Store, options PredecessorOptions, request task.TaskRecord, id string) (prior *task.PriorSession, resultErr error) {
	return ResolvePredecessorContext(context.Background(), store, options, request, id)
}

func ResolvePredecessorContext(ctx context.Context, store *taskdir.Store, options PredecessorOptions, request task.TaskRecord, id string) (prior *task.PriorSession, resultErr error) {
	ctx, lease, err := ordinaryRootAdmission(ctx, store, options.RootAdmission, options.Supervisor.Control)
	if lease != nil && options.RootAdmission == nil {
		defer func() { resultErr = supervisorcore.ReleaseOwnedRootAdmission(lease, resultErr) }()
	}
	if err != nil {
		return nil, err
	}

	if err = task.ValidateTaskID(id); err != nil {
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
	if _, admissionErr := admissioncore.ValidateContinuation(options.Admission, store.Root, *predecessorReq, *predecessorMeta, prior, request); admissionErr != nil {
		return nil, admissionErr
	}
	handedOff, submitted, err := preparePredecessorResolution(ctx, store, td, predecessorReq, predecessorMeta, options.Supervisor)
	if err != nil {
		return nil, err
	}
	if handedOff {
		return prior, nil
	}
	if !submitted {
		return nil, task.ErrRequestConflict
	}
	if err = PredecessorTerminatedContext(ctx, store.Root, td, predecessorReq, SupervisorOptions{AdmissionStore: store, RootAdmission: lease, Options: options.Supervisor.Control, Lookup: options.Supervisor.Lookup(), OpenStore: options.Supervisor.OpenStore, Routing: options.Supervisor.Routing}); err != nil {
		return nil, err
	}
	return prior, nil
}

func ContinuationPrior(td *taskdir.TaskDir, predecessor *task.TaskRecord) (*task.PriorSession, error) {
	prior, _, err := resolveContinuationSession(td, predecessor)
	return prior, err
}

// resolveContinuationSession is the single session resolver for continuation
// admission and observation. The observed provider identity is preferred.
// Only genuine absence of provider.ref.json permits the predecessor's own
// validated immutable PriorSession, which is a requested input rather than an
// observed output. The result is always bound to the immediate predecessor.
func resolveContinuationSession(td *taskdir.TaskDir, predecessor *task.TaskRecord) (*task.PriorSession, ContinuationSessionSource, error) {
	if td == nil || predecessor == nil {
		return nil, "", task.ErrInvariantFault
	}
	identity, absent, err := td.ReadProviderIdentityPresence()
	if err == nil {
		return &task.PriorSession{Provider: identity.Provider, ConversationID: identity.ConversationID, PredecessorTaskID: predecessor.TaskID}, ContinuationSessionObserved, nil
	}
	if absent {
		recorded, _, recordErr := td.PreparedRecords()
		if recordErr != nil {
			return nil, "", recordErr
		}
		if recorded.TaskID != predecessor.TaskID {
			return nil, "", task.ErrIdentityMismatch
		}
		if requested := recorded.PriorSession; requested != nil {
			return &task.PriorSession{Provider: requested.Provider, ConversationID: requested.ConversationID, PredecessorTaskID: recorded.TaskID}, ContinuationSessionRecordedInput, nil
		}
		return nil, "", fmt.Errorf("%w: %w", errPredecessorSessionAbsent, err)
	}
	return nil, "", fmt.Errorf("predecessor session is unavailable: %w", err)
}
