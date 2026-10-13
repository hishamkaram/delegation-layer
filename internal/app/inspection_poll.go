package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	inspectionStatusWatchdog        = time.Second
	inspectionTerminalEventWatchdog = 5 * time.Second
)

// retryableInspectionStatusError preserves a completed but unusable control
// observation while the immutable admission clock still permits another exact
// read. Identity, evidence, and deadline failures never use this wrapper.
type retryableInspectionStatusError struct {
	cause error
}

func (err *retryableInspectionStatusError) Error() string {
	return err.cause.Error()
}

func (err *retryableInspectionStatusError) Unwrap() error {
	return err.cause
}

func inspectionStatusRetryError(err error) error {
	var retry *retryableInspectionStatusError
	if !errors.As(err, &retry) || retry == nil {
		return nil
	}
	return retry.cause
}

func classifiedInspectionStatusError(retry bool, err error) error {
	if !retry || err == nil {
		return err
	}
	return &retryableInspectionStatusError{cause: err}
}

// inspectionPollSchedule limits targeted queue reads while local proof remains
// observable on every admission tick. Completion can prompt one extra read;
// it never substitutes for an independently observed worker exit.
type inspectionPollSchedule struct {
	nextWatchdog     time.Time
	completionSeen   bool
	callbackExpected bool
}

func (schedule *inspectionPollSchedule) due(now time.Time, completed bool) bool {
	prompt := completed && !schedule.completionSeen && !schedule.callbackExpected
	schedule.completionSeen = schedule.completionSeen || completed
	if !schedule.nextWatchdog.IsZero() && now.Before(schedule.nextWatchdog) && !prompt {
		return false
	}
	schedule.nextWatchdog = now.Add(schedule.watchdog())
	return true
}

func (schedule *inspectionPollSchedule) observed(now time.Time, completed bool) {
	schedule.completionSeen = schedule.completionSeen || completed
	schedule.nextWatchdog = now.Add(schedule.watchdog())
}

func (schedule *inspectionPollSchedule) watchdog() time.Duration {
	if schedule.callbackExpected {
		return inspectionTerminalEventWatchdog
	}
	return inspectionStatusWatchdog
}

func inspectionCompletionHint(ctx context.Context, operation *inspection.Operation) (bool, error) {
	_, _, err := operation.ReadCompletedContext(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func pollInspectionStatus(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, identity *pueue.InspectionIdentity) (json.RawMessage, bool, error) {
	var acceptedFacts json.RawMessage
	var acceptedEnded bool
	observation, observeErr := reconcileInspectionObservation(ctx, operation, supervisor, *identity, func(observation pueue.InspectionObservation) error {
		var acceptErr error
		acceptedFacts, acceptedEnded, acceptErr = acceptCompletedInspectionObservation(ctx, operation, supervisor, observation)
		return acceptErr
	})
	if facts, _, terminal, decisionErr := recordedInspectionFactsAfterObservation(ctx, operation); terminal || decisionErr != nil {
		return facts, terminal, decisionErr
	}
	if acceptedEnded {
		return acceptedFacts, acceptedEnded, observeErr
	}
	retry, classifiedErr := classifyInspectionStatusObservation(observation, observeErr)
	if retry {
		return nil, false, classifiedInspectionStatusError(true, classifiedErr)
	}
	if classifiedErr != nil {
		return nil, true, classifiedErr
	}
	if observation.Matched && observation.Job.State == pueue.StateEnded {
		// The observation was already committed under the verified lease when
		// its local evidence was ready. If that evidence was not ready yet,
		// the next watchdog owns the next acceptance attempt.
		return nil, false, nil
	}
	return nil, false, nil
}

// awaitCompletedInspectionObservation gives the observer one independent,
// bounded control-plane window after the admission clock has elapsed. It never
// extends provider work: both eligible completion and the guardian's
// empty-scope termination must already prove they happened before the
// immutable admission deadline.
func awaitCompletedInspectionObservation(operation *inspection.Operation, supervisor *pueue.Client, awaitErr error) (json.RawMessage, error) {
	if !errors.Is(awaitErr, inspection.ErrAdmissionExpired) || operation == nil || supervisor == nil {
		return nil, awaitErr
	}
	proofCtx, cancelProof := context.WithTimeout(context.Background(), pueue.PrivateStatusObservationTimeout)
	facts, _, terminal, proofErr := recordedInspectionFacts(proofCtx, operation)
	cancelProof()
	if terminal || proofErr != nil {
		return facts, proofErr
	}
	observationDeadline := operation.Deadline().Add(pueue.PrivateStatusObservationTimeout)
	if !time.Now().Before(observationDeadline) {
		return nil, awaitErr
	}
	ctx, cancel := context.WithDeadline(context.Background(), observationDeadline)
	defer cancel()
	facts, err := observeCompletedInspection(ctx, operation, supervisor, inFlightInspectionError(awaitErr))
	if err == nil {
		return facts, nil
	}
	if inFlightInspectionError(err) != nil {
		return nil, errors.Join(awaitErr, err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, inspection.ErrAdmissionExpired) {
		return nil, awaitErr
	}
	return nil, err
}

func acceptCompletedInspectionObservation(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, observation pueue.InspectionObservation) (json.RawMessage, bool, error) {
	if facts, _, terminal, err := recordedInspectionFacts(ctx, operation); terminal || err != nil {
		return facts, terminal, err
	}
	if !observation.Matched {
		return nil, false, nil
	}
	if observation.Job.State != pueue.StateEnded {
		identity := observation.Identity
		return acceptInspectionObservation(ctx, operation, supervisor, &identity, observation.Job)
	}
	if !observation.Job.Succeeded {
		identity := observation.Identity
		return acceptInspectionObservation(ctx, operation, supervisor, &identity, observation.Job)
	}
	ready, err := timelyCompletedInspectionEvidence(ctx, operation)
	if err != nil || !ready {
		return nil, false, err
	}
	identity := observation.Identity
	return acceptInspectionObservation(ctx, operation, supervisor, &identity, observation.Job)
}

type completedInspectionAcceptance struct {
	facts json.RawMessage
	ended bool
}

func completedInspectionCommit(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, accepted *completedInspectionAcceptance) pueue.InspectionObservationCommit {
	return func(observation pueue.InspectionObservation) error {
		facts, ended, err := acceptCompletedInspectionObservation(ctx, operation, supervisor, observation)
		accepted.facts = facts
		accepted.ended = ended
		return err
	}
}

type completedInspectionObservationResult struct {
	facts         json.RawMessage
	acceptedEnded bool
	pending       *pueue.Pending
	completed     *pueue.InspectionObservation
	wait          bool
}

type completedInspectionObservationState struct {
	pending   *pueue.Pending
	completed *pueue.InspectionObservation
}

type completedInspectionObservationStep struct {
	facts json.RawMessage
	done  bool
	wait  bool
}

func awaitPendingCompletedInspection(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, pending *pueue.Pending) (completedInspectionObservationResult, error) {
	if err := ctx.Err(); err != nil {
		return completedInspectionObservationResult{}, errors.Join(err, &pueue.InFlightError{Pending: pending})
	}
	identity, err := inspectionIdentityContext(ctx, operation)
	if err != nil {
		return completedInspectionObservationResult{}, err
	}
	accepted := completedInspectionAcceptance{}
	observation, observeErr := supervisor.AwaitInspectionObservationWithLease(ctx, identity, pending, completedInspectionCommit(ctx, operation, supervisor, &accepted))
	if facts, _, terminal, decisionErr := recordedInspectionFactsAfterObservation(ctx, operation); terminal || decisionErr != nil {
		return completedInspectionObservationResult{facts: facts, acceptedEnded: terminal}, decisionErr
	}
	if accepted.ended {
		return completedInspectionObservationResult{facts: accepted.facts, acceptedEnded: true}, observeErr
	}
	if current := inFlightInspectionError(observeErr); current != nil {
		return completedInspectionObservationResult{pending: current}, nil
	}
	retry, classifiedErr := classifyInspectionStatusObservation(observation, observeErr)
	if retry {
		return completedInspectionObservationResult{}, classifiedInspectionStatusError(true, classifiedErr)
	}
	if classifiedErr != nil {
		return completedInspectionObservationResult{}, classifiedErr
	}
	if observation.Matched && observation.Job.State == pueue.StateEnded {
		return completedInspectionObservationResult{completed: &observation}, nil
	}
	return completedInspectionObservationResult{}, nil
}

func (state *completedInspectionObservationState) consumePending(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client) (completedInspectionObservationStep, error) {
	observed, err := awaitPendingCompletedInspection(ctx, operation, supervisor, state.pending)
	state.pending = observed.pending
	if observed.acceptedEnded {
		return completedInspectionObservationStep{facts: observed.facts, done: true}, err
	}
	if err != nil {
		return completedInspectionObservationStep{}, err
	}
	if observed.completed != nil {
		state.completed = observed.completed
	}
	return completedInspectionObservationStep{}, nil
}

func reconcileCompletedInspection(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client) (completedInspectionObservationResult, error) {
	identity, err := inspectionIdentityContext(ctx, operation)
	if err != nil {
		return completedInspectionObservationResult{}, err
	}
	accepted := completedInspectionAcceptance{}
	observation, observeErr := supervisor.ReconcileInspectionWithLease(ctx, identity, completedInspectionCommit(ctx, operation, supervisor, &accepted))
	if facts, _, terminal, decisionErr := recordedInspectionFactsAfterObservation(ctx, operation); terminal || decisionErr != nil {
		return completedInspectionObservationResult{facts: facts, acceptedEnded: terminal}, decisionErr
	}
	if accepted.ended {
		return completedInspectionObservationResult{facts: accepted.facts, acceptedEnded: true}, observeErr
	}
	current := inFlightInspectionError(observeErr)
	if current == nil {
		current = observation.Pending
	}
	if current != nil {
		return completedInspectionObservationResult{pending: current}, nil
	}
	retry, classifiedErr := classifyInspectionStatusObservation(observation, observeErr)
	if retry {
		return completedInspectionObservationResult{}, classifiedInspectionStatusError(true, classifiedErr)
	}
	if classifiedErr != nil {
		return completedInspectionObservationResult{}, classifiedErr
	}
	if !observation.Matched || observation.Job.State != pueue.StateEnded {
		return completedInspectionObservationResult{wait: true}, nil
	}
	return completedInspectionObservationResult{completed: &observation}, nil
}

func commitCompletedInspection(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, observation pueue.InspectionObservation) (completedInspectionObservationResult, error) {
	accepted := completedInspectionAcceptance{}
	commitErr := supervisor.CommitInspectionObservation(ctx, observation, completedInspectionCommit(ctx, operation, supervisor, &accepted))
	if facts, _, terminal, decisionErr := recordedInspectionFactsAfterObservation(ctx, operation); terminal || decisionErr != nil {
		return completedInspectionObservationResult{facts: facts, acceptedEnded: terminal}, decisionErr
	}
	if accepted.ended || commitErr != nil {
		return completedInspectionObservationResult{facts: accepted.facts, acceptedEnded: accepted.ended}, commitErr
	}
	return completedInspectionObservationResult{}, nil
}

func (state *completedInspectionObservationState) observeReady(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client) (completedInspectionObservationStep, error) {
	ready, err := timelyCompletedInspectionEvidence(ctx, operation)
	if err != nil {
		return completedInspectionObservationStep{}, err
	}
	if !ready {
		return completedInspectionObservationStep{wait: true}, nil
	}
	if state.completed != nil {
		committed, commitErr := commitCompletedInspection(ctx, operation, supervisor, *state.completed)
		if committed.acceptedEnded || commitErr != nil {
			return completedInspectionObservationStep{facts: committed.facts, done: true}, commitErr
		}
		return completedInspectionObservationStep{wait: true}, nil
	}
	return state.reconcileAndCommit(ctx, operation, supervisor)
}

func (state *completedInspectionObservationState) reconcileAndCommit(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client) (completedInspectionObservationStep, error) {
	observed, err := reconcileCompletedInspection(ctx, operation, supervisor)
	if observed.acceptedEnded {
		return completedInspectionObservationStep{facts: observed.facts, done: true}, err
	}
	if err != nil {
		return completedInspectionObservationStep{}, err
	}
	if observed.pending != nil {
		state.pending = observed.pending
		return completedInspectionObservationStep{}, nil
	}
	if observed.wait {
		return completedInspectionObservationStep{wait: true}, nil
	}
	committed, commitErr := commitCompletedInspection(ctx, operation, supervisor, *observed.completed)
	if committed.acceptedEnded || commitErr != nil {
		return completedInspectionObservationStep{facts: committed.facts, done: true}, commitErr
	}
	return completedInspectionObservationStep{}, nil
}

func (state *completedInspectionObservationState) observe(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client) (completedInspectionObservationStep, error) {
	if state.pending != nil {
		step, err := state.consumePending(ctx, operation, supervisor)
		if step.done || err != nil {
			return step, err
		}
		if state.pending != nil {
			return completedInspectionObservationStep{}, nil
		}
	}
	if facts, _, terminal, err := recordedInspectionFacts(ctx, operation); terminal || err != nil {
		return completedInspectionObservationStep{facts: facts, done: terminal}, err
	}
	return state.observeReady(ctx, operation, supervisor)
}

func observeCompletedInspection(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, pending *pueue.Pending) (json.RawMessage, error) {
	state := completedInspectionObservationState{pending: pending}
	var lastStatusErr error
	for {
		step, err := state.observe(ctx, operation, supervisor)
		if retryErr := inspectionStatusRetryError(err); retryErr != nil {
			lastStatusErr = retryErr
			if err = waitInspectionObservation(ctx); err != nil {
				return nil, errors.Join(err, lastStatusErr)
			}
			continue
		}
		if step.done || err != nil {
			return step.facts, err
		}
		if !step.wait {
			continue
		}
		if err = waitInspectionObservation(ctx); err != nil {
			return nil, err
		}
	}
}

// classifyInspectionStatusObservation is the single policy boundary shared by
// watchdog polling and terminal-event reconciliation. A valid no-match and a
// completed but unusable status command remain retryable only within the
// caller's immutable observation deadline. The latter retains its cause so a
// persistent control failure is reported at that deadline. Pending commands,
// identity faults, evidence faults, and other policy failures stop immediately.
func classifyInspectionStatusObservation(observation pueue.InspectionObservation, err error) (retry bool, resultErr error) {
	if err == nil {
		return false, nil
	}
	if shouldStopInspectionPolling(err) {
		return false, err
	}
	if errors.Is(err, task.ErrIdentityMismatch) || errors.Is(err, task.ErrEvidenceFault) ||
		errors.Is(err, pueue.ErrBinding) || errors.Is(err, pueue.ErrConfiguration) {
		return false, err
	}
	if errors.Is(err, pueue.ErrStatusTransient) {
		return true, err
	}
	if observation.Pending == nil && isPureErrorSet(err, func(candidate error) bool {
		return errors.Is(candidate, pueue.ErrUnknown)
	}) {
		return true, nil
	}
	return false, err
}
