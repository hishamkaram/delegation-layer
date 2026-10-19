package tasklifecycle

import (
	"errors"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
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
	deadline := time.Now().Add(options.Watch)
	nextSupervisorReconcile := time.Time{}
	for {
		// Each pass owns the derived offer. A finished outcome must never be
		// finalized beside an earlier pass's offer.
		observed.Continuation = nil
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
		passErr := errors.Join(collectErr, inspectErr, descriptorErr, supervisorErr)
		if passErr != nil && !IsPureCollectionPendingError(passErr) {
			return observed, passErr
		}
		// Durable records can change between passes, so every pass re-derives
		// the offer, including its absence.
		if offerErr := refreshContinuation(&observed.Observation, root, td, req, meta, options.Catalog); offerErr != nil {
			return observed, errors.Join(passErr, offerErr)
		}
		if inspectErr == nil && supervisorcore.TaskTerminal(inspection) {
			return observed, nil
		}
		if collectionAttemptEnded(&observed) {
			return observed, nil
		}
		if CollectionWatchExpired(options.Watch, deadline) || !WaitForCollection(deadline) {
			observed.Pending, observed.Waiting = PendingPublication, true
			return observed, nil
		}
	}
}

// refreshContinuation replaces the observation's offer with this pass's
// derived one, including its absence. It reports only a concrete failure.
func refreshContinuation(observed *Observation, root string, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, catalog commonprovider.Catalog) error {
	offer, err := ObserveTimeoutContinuation(root, td, req, meta, catalog)
	observed.Continuation = offer
	if err != nil && !IsPureCollectionPendingError(err) {
		return err
	}
	return nil
}

// collectionAttemptEnded classifies a non-terminal observation that ends this
// collection attempt before its watch deadline.
func collectionAttemptEnded(observed *CollectionResult) bool {
	if observed.Pending == PendingLifetimeFinalization {
		observed.Waiting = true
		return true
	}
	// A closure-proven interruption has no runner left to publish; do not wait.
	if observed.Interrupted() {
		return true
	}
	if InteractionWaiting(observed.Interaction) {
		observed.Pending, observed.Waiting = PendingInteraction, true
		return true
	}
	return false
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
