package admission

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func RunInspection(operation *inspection.Operation, supervisor *pueue.Client, runner, root string) (json.RawMessage, time.Time, error) {
	deadline := operation.Deadline()
	var ctx context.Context
	var cancel context.CancelFunc
	if time.Now().Before(deadline) {
		ctx, cancel = context.WithDeadline(context.Background(), deadline)
	} else {
		// A completed operation remains replayable after its immutable deadline.
		// Give that local proof read one bounded recovery window without renewing
		// the operation or permitting new external work.
		ctx, cancel = context.WithTimeout(context.Background(), inspectionStatusWatchdog)
	}
	defer cancel()
	recordedFacts, _, terminal, recordedErr := RecordedInspectionFacts(ctx, operation)
	if recordedErr != nil {
		if errors.Is(recordedErr, context.DeadlineExceeded) && operation.Expired(time.Now()) {
			return nil, deadline, inspection.ErrAdmissionExpired
		}
		return nil, deadline, recordedErr
	}
	if terminal {
		// A validated worker result and independent empty-scope termination are
		// durable admission facts. They remain replayable after a crash even
		// when ordinary task creation had not happened before the inspection
		// deadline.
		return recordedFacts, deadline, nil
	}
	if operation.Expired(time.Now()) {
		return nil, deadline, inspection.ErrAdmissionExpired
	}
	submission, err := submitInspectionOnceWithState(ctx, operation, supervisor, runner, root)
	if err != nil {
		return nil, deadline, AnnotateMissingInspectionStage("submitting inspection", err)
	}
	facts, err := awaitInspectionWithSubmissionState(ctx, operation, supervisor, submission)
	if err != nil {
		facts, err = awaitCompletedInspectionObservation(operation, supervisor, err)
		if err != nil {
			return nil, deadline, AnnotateMissingInspectionStage("awaiting inspection", err)
		}
		return facts, deadline, nil
	}
	return facts, deadline, nil
}

func awaitInspectionWithSubmissionState(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, submission inspectionSubmissionState) (facts json.RawMessage, resultErr error) {
	defer func() {
		facts, resultErr = finalizeInspectionAwait(ctx, operation, facts, resultErr)
	}()
	state, err := newInspectionAwaitStateWithRetry(ctx, operation, supervisor, submission)
	if err != nil {
		return nil, err
	}
	return observeInspectionUntilTerminal(ctx, operation, supervisor, &state)
}

func finalizeInspectionAwait(ctx context.Context, operation *inspection.Operation, facts json.RawMessage, resultErr error) (json.RawMessage, error) {
	resultErr = normalizeInspectionAwaitError(ctx, operation, resultErr)
	if resultErr == nil {
		return facts, nil
	}
	// Durable inspection proof is the admission authority. An observer can
	// fail after the guardian has already published valid result, lifetime,
	// and Wait evidence, so every error path gets one final local decision
	// before it can reject the dispatch.
	recordedFacts, _, terminal, recordedErr := recordedInspectionFactsAfterObservation(ctx, operation)
	if terminal || recordedErr != nil {
		return recordedFacts, recordedErr
	}
	return facts, resultErr
}

func newInspectionAwaitStateWithRetry(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, submission inspectionSubmissionState) (inspectionAwaitState, error) {
	for {
		state, err := newInspectionAwaitState(ctx, operation, supervisor, submission)
		if err == nil {
			return state, nil
		}
		if !retryInspectionJournalContention(ctx, operation, err) {
			return inspectionAwaitState{}, err
		}
		if err = waitInspectionObservation(ctx); err != nil {
			return inspectionAwaitState{}, err
		}
	}
}

func observeInspectionUntilTerminal(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, state *inspectionAwaitState) (json.RawMessage, error) {
	for {
		facts, ended, observeErr := state.observe(ctx, operation, supervisor)
		if ended || observeErr != nil {
			if retryInspectionJournalContention(ctx, operation, observeErr) {
				if err := waitInspectionObservation(ctx); err != nil {
					return nil, err
				}
				continue
			}
			return facts, observeErr
		}
		if err := waitInspectionObservation(ctx); err != nil {
			return nil, err
		}
	}
}

func retryInspectionJournalContention(ctx context.Context, operation *inspection.Operation, err error) bool {
	return errors.Is(err, task.ErrLockBusy) && operation != nil &&
		!inspectionDeadlineReached(ctx, operation)
}

func (state *inspectionAwaitState) observe(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client) (json.RawMessage, bool, error) {
	if inspectionDeadlineReached(ctx, operation) {
		if facts, _, terminal, err := recordedInspectionFactsAfterObservation(ctx, operation); terminal || err != nil {
			return facts, true, err
		}
		return nil, true, errors.Join(inspection.ErrAdmissionExpired, state.lastStatusErr)
	}
	if facts, recorded, terminal, err := RecordedInspectionFacts(ctx, operation); terminal || err != nil {
		return facts, true, err
	} else if recorded {
		return nil, false, nil
	}
	completed, err := inspectionCompletionHint(ctx, operation)
	if err != nil {
		return nil, true, err
	}
	facts, ended, handled, err := state.observeTerminalEvent(ctx, operation, supervisor, completed)
	if state.rememberRetryableStatus(err) {
		return nil, false, nil
	}
	if ended || err != nil || handled {
		return facts, ended, err
	}
	now := time.Now()
	if !state.polls.due(now, completed) {
		return nil, false, nil
	}
	facts, ended, err = pollInspectionStatus(ctx, operation, supervisor, &state.identity)
	if state.rememberRetryableStatus(err) {
		return nil, false, nil
	}
	return facts, ended, err
}

func (state *inspectionAwaitState) rememberRetryableStatus(err error) bool {
	retryErr := inspectionStatusRetryError(err)
	if retryErr == nil {
		return false
	}
	state.lastStatusErr = retryErr
	return true
}

func newInspectionAwaitState(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, submission inspectionSubmissionState) (inspectionAwaitState, error) {
	identity, err := InspectionIdentityContext(ctx, operation)
	if err != nil {
		return inspectionAwaitState{}, err
	}
	state := inspectionAwaitState{
		identity:                       identity,
		terminalEventRegistrationKnown: submission.terminalEventRegistrationKnown,
		terminalEventRegistered:        submission.terminalEventRegistered,
	}
	if supervisor == nil || identity.NumericTaskID == nil {
		return state, nil
	}
	state.terminalEvents, err = supervisor.PrivateTerminalEventsEnabled(ctx)
	state.polls.callbackExpected = state.terminalEvents && state.terminalEventRegistrationKnown && state.terminalEventRegistered
	return state, err
}

func (state *inspectionAwaitState) observeTerminalEvent(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, completed bool) (json.RawMessage, bool, bool, error) {
	if !state.terminalEvents || state.identity.NumericTaskID == nil {
		return nil, false, false, nil
	}
	if state.terminalEventRegistrationKnown && !state.terminalEventRegistered {
		return nil, false, false, nil
	}
	if state.terminalEventSeen {
		return nil, false, false, nil
	}
	facts, observed, handled, err := acceptInspectionTerminalEventState(ctx, operation, supervisor, *state.identity.NumericTaskID)
	if inspectionStatusRetryError(err) != nil {
		return nil, false, false, err
	}
	if handled {
		state.terminalEventSeen = true
		state.polls.observed(time.Now(), completed)
	}
	if observed || err != nil {
		return facts, true, false, err
	}
	return nil, false, handled, nil
}

func normalizeInspectionAwaitError(ctx context.Context, operation *inspection.Operation, err error) error {
	if err == nil || errors.Is(err, inspection.ErrAdmissionExpired) || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) && !operation.Expired(time.Now()) {
		return err
	}
	return errors.Join(inspection.ErrAdmissionExpired, err)
}

func inspectionDeadlineReached(ctx context.Context, operation *inspection.Operation) bool {
	return ctx.Err() != nil || operation.Expired(time.Now())
}

func waitInspectionObservation(ctx context.Context) error {
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return inspection.ErrAdmissionExpired
	case <-timer.C:
		return nil
	}
}
