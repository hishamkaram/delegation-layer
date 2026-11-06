package infrastructure

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// StageLaunchContext publishes immutable, content-addressed request data in the
// target root. The caller retains the Store and its admission/root boundary.
// This grants no birth, lifecycle or daemon authority. A returned reference is
// preserved alongside a Close fault; callers must retain that fault too.
func StageLaunchContext(ctx context.Context, store *taskdir.Store, selected LaunchContext) (reference LaunchContextReference, resultErr error) {
	if ctx == nil || store == nil {
		return LaunchContextReference{}, os.ErrInvalid
	}
	journal, err := openIntentJournal(ctx, store, true)
	if err != nil {
		return LaunchContextReference{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, journal.Close()) }()
	reference, _, resultErr = journal.stageContext(ctx, selected.Clone())
	return reference, resultErr
}
