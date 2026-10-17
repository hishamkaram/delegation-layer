package tasklifecycle

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

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
	if td == nil || req == nil || req.PriorSession == nil || outcome == nil {
		return nil
	}
	return td.ReleaseSession(req.Provider, req.PriorSession.ConversationID, outcome.EvidenceSHA256)
}

func WaitForCollection(deadline time.Time) bool {
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
	<-timer.C
	return time.Now().Before(deadline)
}

func readProviderSession(td *taskdir.TaskDir) (*task.ProviderRefRecord, error) {
	if td == nil {
		return nil, errors.New("nil task directory")
	}
	session, err := td.ReadProviderIdentity()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return session, err
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
