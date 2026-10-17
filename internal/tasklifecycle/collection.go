package tasklifecycle

import (
	"errors"
	"time"

	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func Collect(root string, td *taskdir.TaskDir, options CollectionOptions) (CollectionResult, error) {
	req, meta, err := td.PreparedRecords()
	if err != nil {
		return CollectionResult{}, err
	}
	return collectPrepared(root, td, req, meta, options)
}

func collectPrepared(root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, options CollectionOptions) (CollectionResult, error) {
	observed := CollectionResult{}
	timeoutAnnotated := false
	deadline := time.Now().Add(options.Watch)
	nextSupervisorReconcile := time.Time{}
	for {
		outcome, collectErr := collectWithInteraction(td, meta.Predicate, &observed.Observation)
		if outcome != nil {
			return finishCollection(observed, root, td, req, meta, options, outcome, collectErr)
		}
		var supervisorErr error
		nextSupervisorReconcile, supervisorErr = reconcileCollectionTimeoutIfActive(deadline, nextSupervisorReconcile, root, td, req, meta, options, &observed.Observation)
		inspection, inspectErr := td.Inspect()
		if inspection != nil {
			observed.Inspection = inspection
		}
		descriptors, descriptorErr := td.LogDescriptors()
		observed.Raw = descriptors
		if winner, terminalErr, handled := CollectionInspectionOutcome(inspection, collectErr, inspectErr, descriptorErr); handled {
			return finishCollection(observed, root, td, req, meta, options, winner, terminalErr)
		}
		timeoutErr := observeTimeoutContinuationOnce(&timeoutAnnotated, &observed.Observation, root, td, req, meta, options.Catalog)
		combinedErr := errors.Join(collectErr, inspectErr, descriptorErr, supervisorErr, timeoutErr)
		if combinedErr != nil && !IsPureCollectionPendingError(combinedErr) {
			return observed, combinedErr
		}
		if inspectErr == nil && supervisorcore.TaskTerminal(inspection) {
			return observed, nil
		}
		if observed.Pending == PendingLifetimeFinalization {
			observed.Waiting = true
			return observed, nil
		}
		if InteractionWaiting(observed.Interaction) {
			observed.Pending, observed.Waiting = PendingInteraction, true
			return observed, nil
		}
		if CollectionWatchExpired(options.Watch, deadline) || !WaitForCollection(deadline) {
			observed.Pending, observed.Waiting = PendingPublication, true
			return observed, nil
		}
	}
}

func finishCollection(observed CollectionResult, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, options CollectionOptions, outcome *task.OutcomeRecord, collectErr error) (CollectionResult, error) {
	supervisorcore.CleanupCompletedTaskFromSaved(root, td, meta, options.Supervisor.Options, options.Supervisor.InitialExecutable)
	completion := &CollectionCompletion{Outcome: outcome, CollectionError: collectErr}
	observed.Completion = completion
	// Optional timeout metadata cannot replace a committed or rejected outcome.
	offer, offerErr := ObserveTimeoutContinuation(root, td, req, meta, options.Catalog)
	if offerErr == nil && offer != nil {
		completion.Continuation = offer
	}
	completion.ReleaseError = ReleaseContinuationSession(td, req, outcome)
	if completion.ReleaseError != nil {
		collectErr = errors.Join(collectErr, completion.ReleaseError)
	}
	publication := task.PublicationRejected
	if outcome.Verdict == task.VerdictCommitted {
		publication = task.PublicationCommitted
	}
	completion.RecoveryAvailable = RejectedRecoveryAvailable(td, options.Registry(), publication.String(), outcome, collectErr)
	return observed, collectErr
}

func CollectAttempt(td *taskdir.TaskDir, predicate task.PredicateRef) (*task.OutcomeRecord, error) {
	outcome, cleanupErr, collectErr := td.Collect(predicate)
	return outcome, errors.Join(cleanupErr, collectErr)
}
