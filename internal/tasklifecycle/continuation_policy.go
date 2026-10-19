package tasklifecycle

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TimeoutContinuation(req *task.TaskRecord, records []taskdir.StopRecord, catalog commonprovider.Catalog, source ContinuationSessionSource, runner, commandRunner string, supervisor *task.SupervisorRef, runnerReleased, launchStateRecorded bool) *ContinuationOffer {
	if req == nil || !HasTerminatedBudgetStop(records) {
		return nil
	}
	launch := continuationLaunch{runner: runner, commandRunner: commandRunner, supervisor: supervisor, stateRecorded: launchStateRecorded}
	return continuationOffer(req, catalog, continuationOfferFacts{cause: ContinuationTimedOut, source: source, launch: launch, runnerReleased: runnerReleased})
}

// continuationLaunch holds the retained coordinates a successor launch needs.
type continuationLaunch struct {
	runner, commandRunner string
	supervisor            *task.SupervisorRef
	stateRecorded         bool
}

// continuationOfferFacts are the observed facts behind one automatic offer.
// An empty source means no provider session reference is available.
type continuationOfferFacts struct {
	cause          ContinuationCause
	source         ContinuationSessionSource
	launch         continuationLaunch
	runnerReleased bool
}

// continuationOffer is the single resumability policy for timed-out and
// interrupted turns. It decides guidance only; continue re-proves handoff.
func continuationOffer(req *task.TaskRecord, catalog commonprovider.Catalog, facts continuationOfferFacts) *ContinuationOffer {
	mode := commonprovider.ContinuationUnsupported
	if registration, err := catalog.Lookup(req.Provider); err == nil {
		mode = registration.Description.Continuation
	}
	sessionAvailable := facts.source != ""
	launch := facts.launch
	resumable := mode == commonprovider.ContinuationNative && sessionAvailable && facts.runnerReleased && launch.runner != "" && launch.stateRecorded
	offer := &ContinuationOffer{Cause: facts.cause, Resumable: resumable, Mode: string(mode), PredecessorID: req.TaskID, SessionSource: facts.source}
	if resumable {
		offer.CommandRunner, offer.Supervisor = launch.commandRunner, launch.supervisor
	}
	offer.Reason = continuationReason(mode, sessionAvailable, facts.runnerReleased, launch.runner, launch.stateRecorded)
	return offer
}

// withUserCancellation suppresses automatic resume guidance after a validated
// user stop. An explicit later continue still proves closure and the session.
func withUserCancellation(offer ContinuationOffer) *ContinuationOffer {
	offer.Resumable, offer.CommandRunner, offer.Supervisor, offer.Cancelled = false, "", nil, true
	offer.Reason = "task was cancelled by the user"
	return &offer
}
