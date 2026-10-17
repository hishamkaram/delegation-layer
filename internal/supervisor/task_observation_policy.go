package supervisor

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// TaskMaintenanceCompletionRefused excludes every independent failure, even
// when joined with a known pre-mutation completion refusal. Pending commands
// and cleanup failures must retain their owners and cannot become deferrals.
func TaskMaintenanceCompletionRefused(err error) bool {
	return task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, pueue.ErrPrivateSupervisorTransitionIncomplete)
	})
}
