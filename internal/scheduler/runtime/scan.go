package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (l *loop) scan(ctx context.Context) error {
	l.refreshObservationRound()
	active, err := l.readActive(ctx)
	if err != nil {
		return err
	}
	bySchedule := make(map[string]contract.Occurrence, len(active))
	activeTasks := make(map[string]bool, len(active))
	for _, occurrence := range active {
		bySchedule[occurrence.ScheduleID] = occurrence
		activeTasks[occurrence.TaskID] = true
	}
	for id := range l.observedRound {
		if !activeTasks[id] {
			delete(l.observedRound, id)
			delete(l.observedDue, id)
		}
	}
	// Traverse (selectionAfter, end], then [start, selectionAfter]. A successful
	// claim changes only the next scan's start, never this scan's page boundary.
	after := l.selectionAfter
	priority := make(map[string]bool)
	if err := l.scanRange(ctx, after, "", bySchedule, priority); err != nil {
		return err
	}
	if after != "" {
		if err := l.scanRange(ctx, "", after, bySchedule, priority); err != nil {
			return err
		}
	}
	l.startObservation(ctx, active, priority)
	return nil
}

// Cadence follows elapsed clock time, even when results repeatedly interrupt
// the idle timer. A rollback starts a fresh round without spinning at one time.
func (l *loop) refreshObservationRound() {
	now := l.config.Clock.Now()
	if l.observationAt.IsZero() || now.Before(l.observationAt) || now.Sub(l.observationAt) >= IdleInterval {
		l.observationRound++
		l.observationAt = now
	}
}

func (l *loop) readActive(ctx context.Context) ([]contract.Occurrence, error) {
	var active []contract.Occurrence
	page := contract.ActivePage{Limit: contract.MaxPageLimit}
	for ctx.Err() == nil {
		result, err := l.config.Store.ActiveOccurrences(ctx, page)
		if err != nil {
			return nil, fmt.Errorf("scheduler active occurrences: %w", err)
		}
		for _, occurrence := range result.Occurrences {
			if err := contract.ValidateOccurrence(occurrence); err != nil {
				return nil, storeError("active occurrence", occurrence, err)
			}
			if occurrence.ScheduleID <= page.AfterScheduleID || occurrence.Release != nil {
				return nil, fmt.Errorf("%w: invalid active occurrence page", task.ErrInvariantFault)
			}
			page.AfterScheduleID = occurrence.ScheduleID
			active = append(active, occurrence)
		}
		if !result.More {
			return active, nil
		}
		if len(result.Occurrences) == 0 {
			return nil, fmt.Errorf("%w: empty active continuation page", task.ErrInvariantFault)
		}
	}
	return nil, ctx.Err()
}

func (l *loop) scanRange(ctx context.Context, after, through string, active map[string]contract.Occurrence, priority map[string]bool) error {
	page := contract.SchedulePage{AfterScheduleID: after, Limit: contract.MaxPageLimit}
	for ctx.Err() == nil {
		result, err := l.config.Store.ListSchedules(ctx, page)
		if err != nil {
			return fmt.Errorf("scheduler list schedules: %w", err)
		}
		for _, summary := range result.Schedules {
			id := summary.Header.ScheduleID
			if through != "" && id > through {
				return nil
			}
			if id <= page.AfterScheduleID {
				return fmt.Errorf("%w: unordered schedule page", task.ErrInvariantFault)
			}
			page.AfterScheduleID = id
			if err := l.consider(ctx, summary, active, priority); err != nil {
				return err
			}
		}
		if !result.More {
			return nil
		}
		if len(result.Schedules) == 0 {
			return fmt.Errorf("%w: empty schedule continuation page", task.ErrInvariantFault)
		}
	}
	return ctx.Err()
}

func (l *loop) consider(ctx context.Context, summary contract.ScheduleSummary, active map[string]contract.Occurrence, priority map[string]bool) error {
	if err := contract.ValidateSummary(summary); err != nil {
		return fmt.Errorf("scheduler metadata %s: %w", summary.Header.ScheduleID, err)
	}
	proceed, err := l.quotaSkip(ctx, &summary)
	if err != nil || !proceed {
		return err
	}
	return l.considerReady(ctx, summary, active, priority)
}

func (l *loop) considerReady(ctx context.Context, summary contract.ScheduleSummary, active map[string]contract.Occurrence, priority map[string]bool) error {
	id := summary.Header.ScheduleID
	if summary.State != contract.StateEnabled || !l.held[id].After(summary.Cursor) {
		delete(l.held, id)
	}
	var prior *contract.Occurrence
	if occurrence, found := active[id]; found {
		prior = &occurrence
	}
	// At most the startup span, unavailable span, and fresh candidate. Each
	// successful span changes the local cursor just as its transaction did.
	for range 4 {
		if err := ctx.Err(); err != nil {
			return err
		}
		decision, err := decideTick(tickInput{
			summary: summary, startup: l.startup,
			now: l.config.Clock.Now().UTC(), held: l.held[id], active: prior, admissionAvailable: l.admissions < AdmissionLimit,
		})
		if err != nil {
			return fmt.Errorf("scheduler recurrence %s: %w", id, err)
		}
		if decision.skip != nil {
			proceed, skipErr := l.skipTick(ctx, &summary, *decision.skip)
			if skipErr != nil || !proceed {
				return skipErr
			}
			continue
		}
		if decision.due.IsZero() {
			return nil
		}
		if decision.hold {
			l.held[id] = decision.due
			priority[prior.TaskID] = true
			return nil
		}
		return l.admit(ctx, summary, decision.due)
	}
	return nil
}

func (l *loop) skipTick(ctx context.Context, summary *contract.ScheduleSummary, span contract.SkipSpan) (bool, error) {
	err := l.recordSkip(ctx, *summary, span)
	if errors.Is(err, errSkipConflict) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	delete(l.held, summary.Header.ScheduleID)
	summary.Cursor = span.To
	return true, nil
}

func (l *loop) recordSkip(ctx context.Context, summary contract.ScheduleSummary, span contract.SkipSpan) error {
	id := summary.Header.ScheduleID
	err := l.config.Store.RecordSkip(ctx, contract.Skip{ScheduleID: id, ExpectedCursor: summary.Cursor, Span: span})
	if err == nil {
		l.event(contract.Occurrence{ScheduleID: id}, "skip", string(span.Reason))
		return nil
	}
	// Pause/delete and cursor races are normal. Read the committed state before
	// returning to metadata discovery; never overwrite its cursor or paused span.
	if _, readErr := l.config.Store.GetSchedule(ctx, id); readErr != nil {
		return fmt.Errorf("scheduler skip readback %s: %w", id, readErr)
	}
	if onlyCauses(err, contract.ErrConflict) {
		return errSkipConflict
	}
	return fmt.Errorf("scheduler skip %s: %w", id, err)
}

// errSkipConflict is consumed by the scan, so a lost skip CAS does not use its
// uncommitted cursor to make another decision in the same pass.
var errSkipConflict = errors.New("scheduler skip state changed")

func (l *loop) startObservation(ctx context.Context, active []contract.Occurrence, priority map[string]bool) {
	if l.reconciling != "" || ctx.Err() != nil {
		return
	}
	start := sort.Search(len(active), func(i int) bool { return active[i].ScheduleID > l.idleAfter })
	for _, dueOnly := range []bool{true, false} {
		for offset := range len(active) {
			occurrence := active[(start+offset)%len(active)]
			if l.live[occurrence.TaskID] || (dueOnly && !priority[occurrence.TaskID]) {
				continue
			}
			freshDue := dueOnly && !l.observedDue[occurrence.TaskID].Equal(l.held[occurrence.ScheduleID])
			if round, observed := l.observedRound[occurrence.TaskID]; observed && round == l.observationRound && !freshDue {
				continue
			}
			l.reconciling = occurrence.TaskID
			l.observedRound[occurrence.TaskID] = l.observationRound
			if dueOnly {
				l.observedDue[occurrence.TaskID] = l.held[occurrence.ScheduleID]
			}
			l.idleAfter = occurrence.ScheduleID
			l.workers.Go(func() {
				state, err := l.config.Tasks.Observe(occurrence.TaskID)
				l.results <- taskResult{occurrence: occurrence, state: state, err: err}
			})
			return
		}
	}
}
