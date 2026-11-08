package infrastructurehost

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Only the fixed host collects command diagnostics into its operation result.
// Ordinary observation retains its historical uncertainty policy. Native Wait
// remains with each command owner; this joins their bootstrap cleanup signal.
func collectInfrastructureBootstrapCleanup(cause error) (pending, releaseFault bool, resultErr error) {
	var owners []*pueue.BootstrapCleanupError
	seen := make(map[*pueue.BootstrapCleanupError]struct{})
	task.AnyErrorNode(cause, func(node error) bool {
		var owner *pueue.BootstrapCleanupError
		if errors.As(node, &owner) && owner != nil {
			if _, found := seen[owner]; !found {
				seen[owner] = struct{}{}
				owners = append(owners, owner)
			}
		}
		return false
	})
	for _, owner := range owners {
		done := owner.Done()
		if done == nil {
			resultErr = errors.Join(resultErr, infrastructure.ErrIdentity)
			pending, releaseFault = true, true
			continue
		}
		<-done
		resultErr = errors.Join(resultErr, owner.Err())
		pending = pending || owner.CleanupPending()
		if !task.AllErrorCauses(errors.Unwrap(owner), func(err error) bool { return errors.Is(err, pueue.ErrInFlight) }) {
			releaseFault = true
		}
	}
	return pending, releaseFault, resultErr
}
