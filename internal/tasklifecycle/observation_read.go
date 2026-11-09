package tasklifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const sessionReleaseContentionWait = 5 * time.Second

func ReadTimeoutRecords(td *taskdir.TaskDir) ([]taskdir.StopRecord, error) {
	if td == nil {
		return nil, errors.New("nil task directory")
	}
	return td.ReadStopRecords()
}

// ReleaseContinuationSession rotates a predecessor session only after the
// current task has a validated terminal winner and its runner lease is gone.
// A nil outcome deliberately retains the claim because there is no durable
// evidence that permits a successor to continue the conversation.
func ReleaseContinuationSession(td *taskdir.TaskDir, req *task.TaskRecord, outcome *task.OutcomeRecord) error {
	return ReleaseContinuationSessionContext(context.Background(), td, req, outcome)
}

func ReleaseContinuationSessionContext(ctx context.Context, td *taskdir.TaskDir, req *task.TaskRecord, outcome *task.OutcomeRecord) error {
	if td == nil || req == nil || req.PriorSession == nil || outcome == nil {
		return nil
	}
	if ctx == nil {
		return context.Canceled
	}
	// TaskDir owns claims and proof checks. Only pure acquisition contention
	// permits waiting; any cleanup fault may retain a lease and must return.
	acquisition, cancel := context.WithTimeout(ctx, sessionReleaseContentionWait)
	defer cancel()
	deadline, _ := acquisition.Deadline()
	for {
		err := td.ReleaseSessionContext(acquisition, req.Provider, req.PriorSession.ConversationID, outcome.EvidenceSHA256)
		if !continuationReleaseContended(err) {
			return err
		}
		if !waitForCollectionContext(acquisition, deadline) {
			return errors.Join(err, acquisition.Err())
		}
	}
}

func continuationReleaseContended(err error) bool {
	// Reuse the whole-error policy; a joined I/O/cleanup/active-owner fault must
	// never become retry permission. A missing seal is not release contention.
	return errors.Is(err, task.ErrLockBusy) && !errors.Is(err, task.ErrNoSeal) && IsPureCollectionPendingError(err)
}

func waitForCollectionContext(ctx context.Context, deadline time.Time) bool {
	if ctx.Err() != nil {
		return false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	if remaining > 20*time.Millisecond {
		remaining = 20 * time.Millisecond
	}
	timer := time.NewTimer(remaining)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil && time.Now().Before(deadline)
	}
}

func reapSupervisorObservation(ctx context.Context, err error) error {
	var inFlight *pueue.InFlightError
	if !errors.As(err, &inFlight) || inFlight.Pending == nil {
		return nil
	}
	if ctx == nil {
		return context.Canceled
	}
	select {
	case <-inFlight.Pending.Done():
		return nil
	case <-ctx.Done():
		// The pending supervisor command retains its own process ownership. The
		// collection caller has reached its finite watch boundary, so leave the
		// observation unresolved and let the caller return code 3.
		return nil
	}
}

func CollectionWatchExpired(watch time.Duration, deadline time.Time) bool {
	return watch <= 0 || time.Now().After(deadline)
}
