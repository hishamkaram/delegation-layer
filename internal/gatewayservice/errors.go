package gatewayservice

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// IsPending reports an unconfirmed stop with no independent fault. A concrete
// observation, identity, configuration or cleanup failure remains a fault even
// when the stop also carries ErrPending.
func IsPending(err error) bool {
	return errors.Is(err, ErrPending) && task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, ErrPending) || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)
	})
}
