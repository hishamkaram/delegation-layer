package taskdir

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// TaskLaunchRefusedContext is the canonical ordinary-task refusal reader.
// Terminal publication and durable user stops remain their own authorities;
// this reader never creates a replacement outcome, deadline, or native fact.
func (td *TaskDir) TaskLaunchRefusedContext(ctx context.Context) (bool, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return false, err
	}
	_, _, meta, specHash, metaHash, err := td.loadAndValidatePreparedSetContext(ctx)
	if err != nil {
		return false, err
	}
	_, err = td.readWinnerRecord(meta, specHash, metaHash)
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, errNoOutcome) {
		return false, err
	}
	return td.UserStopRequestedContext(ctx)
}

func (td *TaskDir) requireTaskLaunchPermissionContext(ctx context.Context) error {
	refused, err := td.TaskLaunchRefusedContext(ctx)
	if err != nil {
		return err
	}
	if refused {
		return task.ErrTerminalTask
	}
	return nil
}

// UserStopRequested is the synchronous guardian callback's durable stop view.
func (td *TaskDir) UserStopRequested() (bool, error) {
	return td.UserStopRequestedContext(context.Background())
}

// UserStopRequestedContext includes observed stops. An acknowledgement never
// revokes the original cancellation or authorizes a later provider launch.
func (td *TaskDir) UserStopRequestedContext(ctx context.Context) (bool, error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return false, err
	}
	records, err := td.ReadStopRecords()
	if err != nil {
		return false, err
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	for _, record := range records {
		if record.Request != nil && record.Request.Cause == "user" {
			return true, nil
		}
	}
	return false, nil
}

// ProveUnlaunchedTaskContext elects the existing lifetime owner before taking
// the run lock. A busy owner is not proof. Durable refusal fences later owners.
func (td *TaskDir) ProveUnlaunchedTaskContext(ctx context.Context) (proven bool, resultErr error) {
	owner, err := td.TryAcquireLifetimeOwnerContext(ctx)
	if errors.Is(err, task.ErrLockBusy) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Release()) }()
	return td.ProveUnlaunchedTaskOwnedContext(ctx)
}

// ProveUnlaunchedTaskOwnedContext requires the caller's lifetime-owner lease.
// It uses the existing run lock and refusal authority; no Wait or termination
// record can truthfully represent a worker that was never launched.
func (td *TaskDir) ProveUnlaunchedTaskOwnedContext(ctx context.Context) (proven bool, resultErr error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return false, err
	}
	if err := td.runLock.LockEXNonblockingContext(ctx); errors.Is(err, task.ErrLockBusy) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.runLock.Unlock()) }()
	refused, err := td.TaskLaunchRefusedContext(ctx)
	if err != nil || !refused {
		return false, err
	}
	evidence, err := LoadLifetimeEvidenceContext(ctx, td)
	if err != nil {
		return false, err
	}
	if evidence.Intent != nil || evidence.Registration != nil || evidence.Worker != nil || evidence.WorkerWait != nil || evidence.Termination != nil || len(evidence.Launches) != 0 {
		return false, nil
	}
	var started task.ProviderStartRecord
	err = td.store.readRecordContext(ctx, filepath.Join(td.Dir, "provider.start"), &started)
	if task.RecordAbsent(err) {
		return true, nil
	}
	return false, err
}
