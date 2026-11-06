package control

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// CheckPrivateAdmissionRuntime observes the canonical readiness predicate
// under the shared generation lease. Only a known mismatch refuses early;
// incomplete facts remain for the existing owners and final SubmissionGate.
// This observation neither mutates state nor grants continuing admission.
func CheckPrivateAdmissionRuntime(ctx context.Context, root string, runtime task.SupervisorRef) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !IsPrivateConfig(root, runtime.ConfigPath) {
		return ErrConfiguration
	}
	if err := validatePrivateSupervisorRef(runtime); err != nil {
		return errors.Join(ErrBinding, err)
	}
	return WithSupervisorLeaseContext(ctx, root, func(locked context.Context) error {
		facts, err := loadPrivateGenerationFacts(root)
		if err != nil {
			return errors.Join(err, locked.Err())
		}
		refused, err := facts.runtimeRefused(runtime)
		if err != nil {
			return errors.Join(err, locked.Err())
		}
		if err = locked.Err(); err != nil {
			return err
		}
		if refused {
			return ErrPrivateSupervisorSubmissionIncomplete
		}
		return nil
	})
}
