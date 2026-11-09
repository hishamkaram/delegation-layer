package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (l *loop) admit(ctx context.Context, summary contract.ScheduleSummary, due time.Time) error {
	candidate, ready, err := l.prepareCandidate(ctx, summary, due)
	if err != nil || !ready {
		return err
	}
	claim := candidate.claim
	if l.live[claim.TaskID] || l.reconciling == claim.TaskID {
		return fmt.Errorf("%w: scheduler reused live task %s", task.ErrInvariantFault, claim.TaskID)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Recheck after definition/ID preparation, immediately before the short
	// claim transaction. Clock rollback also cannot authorize an early claim.
	now := l.config.Clock.Now().UTC()
	if now.Before(due) || !now.Before(due.Add(ClaimWindow)) {
		return nil
	}
	l.live[claim.TaskID] = true
	l.admissions++
	authorized, err := l.claim(ctx, claim)
	if err != nil || !authorized {
		delete(l.live, claim.TaskID)
		l.admissions--
		return err
	}
	delete(l.held, claim.ScheduleID)
	l.selectionAfter = claim.ScheduleID
	occurrence := claimOccurrence(claim)
	request := DispatchRequest{TaskID: claim.TaskID, Definition: candidate.definition}
	l.event(occurrence, "claim", "armed")
	l.workers.Go(func() {
		state, dispatchErr := l.config.Tasks.Dispatch(request)
		if state.Attempt != nil {
			if store, ok := l.config.Store.(contract.ReliabilityStore); ok {
				saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				saveErr := store.RecordAttempt(saveCtx, request.TaskID, *state.Attempt)
				cancel()
				if saveErr != nil {
					dispatchErr = errors.Join(dispatchErr, ErrAttemptPersistence, saveErr)
				}
			}
		}
		l.results <- taskResult{occurrence: occurrence, state: state, err: dispatchErr, dispatch: true}
	})
	return nil
}

type admissionCandidate struct {
	claim      contract.Claim
	definition contract.Definition
}

func (l *loop) prepareCandidate(ctx context.Context, summary contract.ScheduleSummary, due time.Time) (admissionCandidate, bool, error) {
	id := summary.Header.ScheduleID
	schedule, err := l.config.Store.GetSchedule(ctx, id)
	if err != nil {
		return admissionCandidate{}, false, fmt.Errorf("scheduler definition %s: %w", id, err)
	}
	if validationErr := contract.ValidateSchedule(schedule); validationErr != nil {
		return admissionCandidate{}, false, validationErr
	}
	if digestErr := contract.VerifyDefinitionDigests(schedule.Definition, summary.Digests); digestErr != nil {
		return admissionCandidate{}, false, digestErr
	}
	if schedule.State != contract.StateEnabled || !schedule.Cursor.Equal(summary.Cursor) {
		return admissionCandidate{}, false, nil
	}
	taskID, err := l.config.NewTaskID()
	if err != nil {
		return admissionCandidate{}, false, fmt.Errorf("scheduler task identity: %w", err)
	}
	claim := contract.Claim{
		ScheduleID: id, ExpectedCursor: schedule.Cursor, Due: due,
		TaskID: taskID, ArmedAt: l.config.Clock.Now().UTC(),
	}
	if validationErr := contract.ValidateClaim(claim); validationErr != nil {
		return admissionCandidate{}, false, validationErr
	}
	return admissionCandidate{claim: claim, definition: schedule.Definition.Clone()}, true, nil
}

func (l *loop) claim(ctx context.Context, claim contract.Claim) (bool, error) {
	for attempt := range 2 {
		result, claimErr := l.config.Store.ClaimAndArm(ctx, claim)
		decision, err := l.resolveClaim(ctx, claim, result, claimErr)
		if err != nil {
			return false, err
		}
		if !decision.retry {
			return decision.invoke, nil
		}
		if attempt == 1 {
			return false, storeError("claim", claimOccurrence(claim), claimErr)
		}
	}
	return false, fmt.Errorf("%w: claim resolution exhausted", task.ErrInvariantFault)
}

type claimDecision struct{ invoke, retry bool }

func (l *loop) resolveClaim(ctx context.Context, claim contract.Claim, result contract.ClaimResult, claimErr error) (claimDecision, error) {
	switch result.Outcome {
	case contract.ClaimNewlyCommitted:
		if claimErr != nil || result.TaskID != claim.TaskID {
			return claimDecision{}, fmt.Errorf("%w: invalid newly committed claim result", task.ErrInvariantFault)
		}
		return claimDecision{invoke: true}, nil
	case contract.ClaimExisting:
		return claimDecision{}, l.existingClaim(ctx, claim, result.TaskID, claimErr)
	case contract.ClaimUnknown:
		retry, err := l.resolveUnknown(ctx, claim, claimErr)
		return claimDecision{retry: retry}, err
	default:
		return claimDecision{}, fmt.Errorf("%w: invalid claim outcome", task.ErrInvariantFault)
	}
}

func (l *loop) resolveUnknown(ctx context.Context, claim contract.Claim, claimErr error) (bool, error) {
	stored, readErr := l.config.Store.GetOccurrence(ctx, claim.TaskID)
	if errors.Is(claimErr, task.ErrInvariantFault) {
		return false, storeError("claim", claimOccurrence(claim), claimErr)
	}
	if readErr == nil {
		if !sameOccurrence(stored, claimOccurrence(claim)) {
			return false, fmt.Errorf("%w: conflicting claim readback %s", task.ErrInvariantFault, claim.TaskID)
		}
		l.event(stored, "claim", "uncertain_commit")
		return false, nil // A readback NEVER authorizes dispatch.
	}
	if !onlyCauses(readErr, fs.ErrNotExist) {
		return false, storeError("claim readback", claimOccurrence(claim), readErr)
	}
	if claimErr != nil && onlyCauses(claimErr, contract.ErrConflict) {
		_, readErr = l.config.Store.GetSchedule(ctx, claim.ScheduleID)
		return false, readErr
	}
	if claimErr == nil {
		return false, fmt.Errorf("%w: unknown claim without error", task.ErrInvariantFault)
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	now := l.config.Clock.Now().UTC()
	if now.Before(claim.Due) || !now.Before(claim.Due.Add(ClaimWindow)) {
		return false, storeError("claim", claimOccurrence(claim), claimErr)
	}
	return true, nil
}

func (l *loop) existingClaim(ctx context.Context, claim contract.Claim, taskID string, claimErr error) error {
	if claimErr != nil {
		return storeError("existing claim", claimOccurrence(claim), claimErr)
	}
	stored, err := l.config.Store.GetOccurrence(ctx, taskID)
	if err != nil {
		return storeError("existing claim readback", claimOccurrence(claim), err)
	}
	if stored.ScheduleID != claim.ScheduleID || !stored.Due.Equal(claim.Due) {
		return fmt.Errorf("%w: conflicting existing occurrence %s", task.ErrInvariantFault, taskID)
	}
	return nil
}
