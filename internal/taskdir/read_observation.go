package taskdir

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// WithExistingReadObservation borrows the existing maintenance inode once for
// a read callback. Nested canonical reads share this descriptor's SH lease;
// callers must still use nonblocking operation/owner observations. It grants
// no mutation, closure or process authority and never creates storage.
func (s *Store) WithExistingReadObservation(ctx context.Context, observe func(context.Context) error) (resultErr error) {
	if s == nil || s.maintLock == nil || ctx == nil {
		return task.ErrInvalidPermit
	}
	if observe == nil {
		return errors.New("missing read observation")
	}
	if err := s.maintLock.LockSHNonblockingContext(ctx); err != nil {
		return err
	}
	defer s.finishConnectorMaintenance(&resultErr)
	return observe(ctx)
}
