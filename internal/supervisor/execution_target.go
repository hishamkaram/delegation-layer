package supervisor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

// ObserveAdmissionExecutionTarget reports, read-only, the supervisor process
// that would run work admitted with selection. It applies the canonical
// private-or-external choice and never binds, starts, repairs, migrates, or
// locks. An explicit or launch-scope supervisor config is external, so its
// target is unknown. Every unknown result is an error wrapping
// pueue.ErrExecutionTargetUnverified; callers treat it as advisory.
func ObserveAdmissionExecutionTarget(ctx context.Context, selection AdmissionSelection, root string, runtime AdmissionOptions) (pueue.ExecutionTarget, error) {
	private, err := AdmissionUsesPrivateSupervisor(selection, root)
	if err != nil {
		return pueue.ExecutionTarget{}, errors.Join(pueue.ErrExecutionTargetUnverified, err)
	}
	if !private {
		return pueue.ExecutionTarget{}, fmt.Errorf("%w: external supervisor config has no verified execution target", pueue.ErrExecutionTargetUnverified)
	}
	return pueue.ObservePrivateExecutionTarget(ctx, root, runtime.Control)
}
