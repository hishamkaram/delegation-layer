package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ResultReader is deliberately separate from Tasks.Observe: this port reads
// existing local evidence and cannot fence, collect, or repair a task.
type (
	ResultReader interface {
		ReadResult(context.Context, contract.Occurrence) (contract.ResultFact, error)
	}
	HookRunner interface {
		RunHook(context.Context, contract.FailureHook, contract.Notification) (string, error)
	}
)

type reliabilityResult struct {
	after             string
	more              bool
	notificationAfter string
	notificationMore  bool
	err               error
}

func (l *loop) startReliability(ctx context.Context) {
	store, ok := l.config.Store.(contract.ReliabilityStore)
	if !ok || l.config.Evidence == nil || l.reliabilityRunning || l.config.Clock.Now().Before(l.reliabilityAt) {
		return
	}
	l.reliabilityRunning = true
	after := l.reliabilityAfter
	notificationAfter := l.notificationAfter
	l.workers.Go(func() {
		result := l.reconcilePage(ctx, store, after, notificationAfter)
		// Exactly one reliability worker exists and this mailbox has its own slot.
		// Shutdown may join before receiving; the send cannot depend on the loop.
		l.reliabilityResults <- result
	})
}

func (l *loop) reconcilePage(ctx context.Context, store contract.ReliabilityStore, after, notificationAfter string) reliabilityResult {
	pageCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	occurrences, more, err := store.ReconciliationPage(pageCtx, "", after, 16)
	if err != nil {
		return reliabilityResult{err: err}
	}
	for _, o := range occurrences {
		after = o.TaskID
		f, readErr := l.config.Evidence.ReadResult(pageCtx, o)
		if readErr != nil {
			l.event(o, "health", "evidence_unavailable")
			if fatalResultReadError(readErr) {
				return reliabilityResult{err: readErr}
			}
			if pageCtx.Err() != nil {
				return reliabilityResult{after: after, more: more, err: pageCtx.Err()}
			}
			continue
		}
		if f.TaskID != o.TaskID {
			return reliabilityResult{err: task.ErrIdentityMismatch}
		}
		if recordErr := store.RecordResult(pageCtx, f); recordErr != nil {
			return reliabilityResult{err: recordErr}
		}
		if holdErr := store.ConsiderQuotaHold(pageCtx, o.TaskID, l.config.Clock.Now().UTC()); holdErr != nil {
			return reliabilityResult{err: holdErr}
		}
	}
	// Notification claims are independently paged over persisted health, so a
	// crash after projection but before claiming cannot permanently lose an alert.
	schedules, err := l.config.Store.ListSchedules(pageCtx, contract.SchedulePage{AfterScheduleID: notificationAfter, Limit: 16})
	if err != nil {
		return reliabilityResult{err: err}
	}
	for _, summary := range schedules.Schedules {
		notificationAfter = summary.Header.ScheduleID
		if err := l.notify(ctx, store, summary.Header.ScheduleID); err != nil {
			return reliabilityResult{err: err}
		}
	}
	return reliabilityResult{after: after, more: more, notificationAfter: notificationAfter, notificationMore: schedules.More}
}

func (l *loop) notify(ctx context.Context, store contract.ReliabilityStore, id string) error {
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	schedule, err := l.config.Store.GetSchedule(readCtx, id)
	if err != nil {
		return err
	}
	if schedule.Definition.Hook == nil {
		return nil
	}
	eventID, err := l.config.NewNotificationID()
	if err != nil {
		return err
	}
	notice, err := store.ClaimNotification(readCtx, id, eventID, l.config.Clock.Now().UTC())
	if err != nil || notice == nil {
		return err
	}
	result := "unavailable"
	if l.config.Hooks != nil {
		hookCtx, stop := context.WithTimeout(ctx, schedule.Definition.Hook.Timeout)
		result, err = l.config.Hooks.RunHook(hookCtx, *schedule.Definition.Hook, *notice)
		stop()
		if err != nil {
			l.event(contract.Occurrence{ScheduleID: id, TaskID: notice.LastTaskID}, "hook", "hook_failed")
		}
	}
	if validationErr := contract.ValidateNotificationResult(result); validationErr != nil {
		result = "uncertain"
	}
	saveCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	return store.FinishNotification(saveCtx, eventID, result)
}

func (l *loop) finishReliability(result reliabilityResult) error {
	l.reliabilityRunning = false
	l.reliabilityAfter = result.after
	l.notificationAfter = result.notificationAfter
	if !result.notificationMore {
		l.notificationAfter = ""
	}
	if !result.more {
		l.reliabilityAfter = ""
	}
	if !result.more && !result.notificationMore {
		l.reliabilityAt = l.config.Clock.Now().Add(IdleInterval)
	}
	if result.err != nil && !cancellationOnly(result.err) {
		return fmt.Errorf("scheduler reliability reconciliation: %w", result.err)
	}
	return nil
}

func (l *loop) quotaSkip(ctx context.Context, summary *contract.ScheduleSummary) (bool, error) {
	if summary.State != contract.StateEnabled {
		return true, nil
	}
	store, ok := l.config.Store.(contract.ReliabilityStore)
	if !ok {
		return true, nil
	}
	if err := store.ReconsiderQuotaHolds(ctx, summary.Header.ScheduleID, l.config.Clock.Now().UTC()); err != nil {
		return false, err
	}
	hold, err := store.QuotaHold(ctx, summary.Header.ScheduleID)
	if err != nil || hold == nil {
		return true, err
	}
	end := hold.Until
	now := l.config.Clock.Now().UTC()
	if now.Before(end) {
		return false, nil // Persist the gap once at expiry or explicit early resume.
	}
	if !end.After(summary.Cursor) {
		return true, nil
	}
	skip := contract.Skip{ScheduleID: summary.Header.ScheduleID, ExpectedCursor: summary.Cursor, Span: contract.SkipSpan{From: summary.Cursor, To: end, Reason: contract.SkipQuotaExhausted}}
	return l.skipTick(ctx, summary, skip.Span)
}

// Root identity and retained cleanup owners affect the whole runtime. Other
// read faults remain confined to the occurrence whose evidence is unavailable.
func fatalResultReadError(err error) bool {
	return errors.Is(err, ErrRootIdentity) || errors.Is(err, ErrTaskCleanupUnresolved)
}
