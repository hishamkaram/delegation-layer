package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const lifetimeRecoveryTimeout = 10 * time.Second

var errLifetimeRecoveryPending = errors.New("task lifetime recovery is owned by an active guardian")

// recoverTaskLifetime is the sole ordinary-task recovery policy. It runs only
// after the saved scheduler row has ended. A busy owner means the guardian is
// still completing its transition; an observer never competes with it.
func recoverTaskLifetime(parent context.Context, td *taskdir.TaskDir) (resultErr error) {
	if parent == nil {
		return context.Canceled
	}
	if td == nil {
		return task.ErrEvidenceFault
	}
	intent, err := optionalLifetimeIntent(td.ReadLifetimeIntentContext(parent))
	if err != nil {
		return err
	}
	registration, err := optionalLifetimeRegistration(td.ReadLifetimeRegistrationContext(parent))
	if err != nil {
		return err
	}
	if intent == nil && registration == nil {
		return nil
	}
	owner, err := td.TryAcquireLifetimeOwnerContext(parent)
	if errors.Is(err, task.ErrLockBusy) {
		return errLifetimeRecoveryPending
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, owner.Release()) }()
	ctx, cancel := context.WithTimeout(parent, lifetimeRecoveryTimeout)
	defer cancel()
	return reconcileTaskLifetimeOwned(ctx, td)
}

// reconcileTaskLifetimeOwned applies the ordinary-task recovery policy while
// its caller retains the lifetime-owner lease. Live guardians use the same
// transition after a deterministic native-scope collision.
func reconcileTaskLifetimeOwned(ctx context.Context, td *taskdir.TaskDir) error {
	if ctx == nil {
		return context.Canceled
	}
	if td == nil {
		return task.ErrEvidenceFault
	}
	return reconcileLifetimeRecordsOwned(ctx, td)
}

type lifetimeTerminationRecorder interface {
	RecordLifetimeTerminationContext(context.Context, string) (*task.LifetimeTerminationRecord, error)
}

func recordTaskLifetimeTerminationContext(ctx context.Context, recorder lifetimeTerminationRecorder, cause string) (*task.LifetimeTerminationRecord, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	return recorder.RecordLifetimeTerminationContext(ctx, cause)
}

func recoverLifetimeScope(ctx context.Context, scope lifetimeFinalizationScope, refs []lifetime.LaunchRef, assurance string) error {
	if assurance != task.LifetimeAssuranceExact {
		if len(refs) == 0 {
			return fmt.Errorf("%w: cooperative scope has no durable launch identity", lifetime.ErrUnverifiableLaunch)
		}
		return errors.Join(
			scope.Terminate(ctx, refs),
			fmt.Errorf("%w: cooperative recovery has no live owner proof", lifetime.ErrUnverifiableLaunch),
		)
	}
	return finalizeLifetimeScope(ctx, scope, refs)
}
