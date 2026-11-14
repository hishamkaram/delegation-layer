package admission

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ContinuationConnectorContext derives the successor's execution constraint
// from the predecessor's immutable path binding. Gateway correlation belongs
// to each caller and is deliberately outside this rule. Legacy predecessors
// retain their existing continuation behavior.
func ContinuationConnectorContext(predecessor, requested *task.ConnectorContext) (*task.ConnectorContext, error) {
	if predecessor == nil || !predecessor.Project.IsPath() {
		return requested.Clone(), nil
	}
	if requested == nil {
		return predecessor.Clone(), nil
	}
	if requested.RootToken != predecessor.RootToken || requested.Project != predecessor.Project || !continuationWorkspaceMatches(requested.WorkspaceConstraint, predecessor.WorkspaceConstraint) {
		return nil, fmt.Errorf("continuation must preserve predecessor path binding: %w", task.ErrRequestConflict)
	}
	return requested.Clone(), nil
}

func continuationWorkspaceMatches(requested, predecessor *task.WorkspaceConstraint) bool {
	if requested == nil || predecessor == nil {
		return requested == predecessor
	}
	return *requested == *predecessor
}

// ValidateContinuation constructs the exact successor identity and applies
// the shared static admission gate before any continuation recovery. The
// predecessor's immutable records decide provider compatibility and preserve
// its path execution constraint. The provider candidate is compiled in the
// successor's own launch scope, the one
// options.Profiles already carries. A predecessor environment is never a
// fallback.
func ValidateContinuation(options StaticOptions, root string, predecessor task.TaskRecord, predecessorMeta task.MetaRecord, prior *task.PriorSession, successor task.TaskRecord) (task.TaskRecord, error) {
	binding, err := ContinuationConnectorContext(predecessor.ConnectorContext, successor.ConnectorContext)
	if err != nil {
		return task.TaskRecord{}, err
	}
	if successor.ConnectorContext == nil && binding != nil {
		return task.TaskRecord{}, fmt.Errorf("continuation is missing predecessor path binding: %w", task.ErrRequestConflict)
	}
	if err = task.ValidateConnectorBinding(&successor); err != nil {
		return task.TaskRecord{}, err
	}
	if prior == nil {
		return task.TaskRecord{}, errors.New("predecessor session is unavailable")
	}
	priorCopy := *prior
	successor.PriorSession = &priorCopy
	if err := options.Catalog.ValidateContinuation(predecessor, predecessorMeta, successor); err != nil {
		return task.TaskRecord{}, err
	}
	if err := ValidateStatic(options, root, successor); err != nil {
		return task.TaskRecord{}, err
	}
	return successor, nil
}
