package supervisor

import (
	"context"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// WithAdmissionCreation holds the existing generation SH from its final gate
// through the caller's immutable creation commit. Callers release this scope
// before capture, submission or any operation which may need bootstrap EX.
// Logical binding selects the private owner; runtime selects actual capability
// evidence. Canonical generation and scheduling
// records remain the authorities, not an earlier preparation observation.
func WithAdmissionCreation(ctx context.Context, root string, logical, runtime task.SupervisorRef, create func(context.Context) error) error {
	if ctx == nil || create == nil {
		return os.ErrInvalid
	}
	if !pueue.IsPrivateConfig(root, logical.ConfigPath) {
		return create(ctx)
	}
	if !pueue.IsPrivateConfig(root, runtime.ConfigPath) {
		return pueue.ErrBinding
	}
	return pueue.WithSupervisorLeaseContext(ctx, root, func(locked context.Context) error {
		if _, err := SubmissionGate(root)(locked, runtime); err != nil {
			return err
		}
		return create(locked)
	})
}
