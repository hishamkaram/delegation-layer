package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (l *loop) finish(ctx context.Context, result taskResult) error {
	if result.dispatch {
		delete(l.live, result.occurrence.TaskID)
		l.admissions--
	} else {
		l.reconciling = ""
	}
	phase := "observe"
	if result.dispatch {
		phase = "dispatch"
	}
	if err := l.reportTaskError(result); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if result.state.Observation.ObservedAt.IsZero() {
		if result.err != nil && result.state.Release == nil {
			return nil
		}
		return fmt.Errorf("%w: missing task observation %s", task.ErrInvariantFault, result.occurrence.TaskID)
	}
	if err := contract.ValidateObservation(result.state.Observation); err != nil {
		return storeError("invalid observation", result.occurrence, errors.Join(task.ErrInvariantFault, err))
	}
	if err := l.recordObservation(ctx, result.occurrence, result.state.Observation); err != nil {
		return err
	}
	if result.state.Observation.FaultCode != "" {
		l.event(result.occurrence, phase, result.state.Observation.FaultCode)
	}
	if result.state.Release != nil {
		release := *result.state.Release
		if err := contract.ValidateRelease(release); err != nil {
			return storeError("invalid release", result.occurrence, errors.Join(task.ErrInvariantFault, err))
		}
		occurrence := result.occurrence
		occurrence.Release = &release
		if result.state.Attempt != nil {
			occurrence.Attempt = result.state.Attempt
		}
		if err := l.projectBeforeRelease(ctx, occurrence); err != nil {
			return err
		}
		return l.recordRelease(ctx, result.occurrence, release)
	}
	l.event(result.occurrence, phase, "observed")
	return nil
}

func (l *loop) reportTaskError(result taskResult) error {
	if result.state.Attempt != nil && l.config.Report != nil {
		f := result.state.Attempt
		l.config.Report(Event{ScheduleID: result.occurrence.ScheduleID, TaskID: result.occurrence.TaskID, Phase: "admission", Code: f.Code, Stage: f.Stage, NextAction: f.NextAction})
	}
	if result.err == nil {
		return nil
	}
	phase := "observe"
	if result.dispatch {
		phase = "dispatch"
	}
	l.event(result.occurrence, phase, "task_error")
	if errors.Is(result.err, ErrRootIdentity) || errors.Is(result.err, ErrTaskCleanupUnresolved) || errors.Is(result.err, ErrAttemptPersistence) {
		return storeError(phase, result.occurrence, result.err)
	}
	return nil
}

func (l *loop) readback(ctx context.Context, original contract.Occurrence) (contract.Occurrence, error) {
	stored, err := l.config.Store.GetOccurrence(ctx, original.TaskID)
	if err != nil {
		return contract.Occurrence{}, storeError("readback", original, err)
	}
	if !sameOccurrence(stored, original) {
		return contract.Occurrence{}, fmt.Errorf("%w: changed occurrence identity %s", task.ErrInvariantFault, original.TaskID)
	}
	return stored, nil
}

func (l *loop) recordObservation(ctx context.Context, occurrence contract.Occurrence, observation contract.Observation) error {
	candidate := contract.OccurrenceObservation{TaskID: occurrence.TaskID, Observation: observation}
	for attempt := range 2 {
		err := l.config.Store.RecordObservation(ctx, candidate)
		if err == nil {
			return nil
		}
		stored, readErr := l.readback(ctx, occurrence)
		if readErr != nil {
			return readErr
		}
		if errors.Is(err, task.ErrInvariantFault) {
			return storeError("observation", occurrence, err)
		}
		if stored.Observation != nil && !stored.Observation.ObservedAt.Before(observation.ObservedAt) {
			return nil // Store owns monotonic cache replacement, including ties.
		}
		if onlyCauses(err, contract.ErrConflict) {
			l.event(occurrence, "observe", "conflict")
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt == 1 {
			return storeError("observation", occurrence, err)
		}
	}
	return nil
}

func (l *loop) recordRelease(ctx context.Context, occurrence contract.Occurrence, release contract.Release) error {
	candidate := contract.OccurrenceRelease{TaskID: occurrence.TaskID, Release: release}
	for attempt := range 2 {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := l.config.Store.Release(ctx, candidate)
		if err == nil {
			l.event(occurrence, "release", string(release.Kind))
			return nil
		}
		stored, readErr := l.readback(ctx, occurrence)
		if readErr != nil {
			return readErr
		}
		if errors.Is(err, task.ErrInvariantFault) {
			return storeError("release", occurrence, err)
		}
		if stored.Release != nil {
			if repeatErr := contract.CheckRepeatedRelease(*stored.Release, release); repeatErr != nil {
				l.event(occurrence, "release", "conflict")
			}
			return nil // The immutable winner is never overwritten.
		}
		if attempt == 1 || onlyCauses(err, contract.ErrConflict) {
			return storeError("release", occurrence, err)
		}
	}
	return nil
}

// Admission cannot pass the reservation until the current outcome's quota
// advice has been installed. Late publication is revisited separately.
func (l *loop) projectBeforeRelease(ctx context.Context, o contract.Occurrence) error {
	store, ok := l.config.Store.(contract.ReliabilityStore)
	if !ok || l.config.Evidence == nil {
		return nil
	}
	fact, err := l.config.Evidence.ReadResult(ctx, o)
	if err != nil {
		return storeError("release projection", o, err)
	}
	if fact.TaskID != o.TaskID {
		return task.ErrIdentityMismatch
	}
	if err = store.RecordResult(ctx, fact); err != nil {
		return storeError("release projection", o, err)
	}
	return store.ConsiderQuotaHold(ctx, o.TaskID, l.config.Clock.Now().UTC())
}
