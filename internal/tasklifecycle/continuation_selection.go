package tasklifecycle

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The guarded predecessor owns the execution constraint. A caller may repeat
// its value, but may neither retarget it nor retain mutable ownership of it.
func guardedContinuationContext(saved, requested *task.ConnectorContext) (*task.ConnectorContext, error) {
	if saved == nil {
		return nil, fmt.Errorf("guarded predecessor has no connector context: %w", task.ErrRequestConflict)
	}
	if requested != nil && !sameGuardedContinuationContext(*saved, *requested) {
		return nil, fmt.Errorf("guarded continuation must preserve the saved connector context: %w", task.ErrRequestConflict)
	}
	return saved.Clone(), nil
}

func sameGuardedContinuationContext(saved, requested task.ConnectorContext) bool {
	if saved.RootToken != requested.RootToken || saved.Project != requested.Project {
		return false
	}
	if saved.WorkspaceConstraint == nil || requested.WorkspaceConstraint == nil {
		return saved.WorkspaceConstraint == requested.WorkspaceConstraint
	}
	return *saved.WorkspaceConstraint == *requested.WorkspaceConstraint
}

func validateContinuationSelection(request DispatchRequest, options ContinuationOptions) error {
	selection := options.PredecessorGuard
	if selection == nil {
		return nil
	}
	if err := selection.Validate(); err != nil {
		return err
	}
	if selection.SpecSHA256 == "" {
		return errors.New("guarded continuation requires the expected predecessor spec SHA256")
	}
	if options.SuccessorTaskID == "" || options.SuccessorTaskID == request.TaskID {
		return errors.New("guarded continuation requires a distinct caller-selected successor task ID")
	}
	if request.Origin == nil {
		return errors.New("guarded continuation requires a fresh successor origin")
	}
	if err := task.ValidateGatewayOrigin(*request.Origin); err != nil {
		return err
	}
	prior := selection.Origin
	if request.Origin.GatewayID == prior.GatewayID && request.Origin.AdmissionGeneration == prior.AdmissionGeneration && request.Origin.OperationID == prior.OperationID {
		return task.ErrRequestConflict
	}
	return nil
}
