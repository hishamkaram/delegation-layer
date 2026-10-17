package tasklifecycle

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// sameExistingRequest preserves the immutable request recorded by an earlier
// dispatch when a retry omits the budget. Defaults are resolved when a new
// task is admitted, but an omitted retry option must remain idempotent across a
// default change; an explicit budget still has to match exactly.
func SameExistingRequest(requestedBudget string, request, existing *task.TaskRecord) bool {
	if task.CompareRequests(request, existing) {
		return true
	}
	if requestedBudget == "" && request != nil && existing != nil {
		candidate := *request
		candidate.BudgetNanos = existing.BudgetNanos
		candidate.RequestedConfig.Budget = existing.RequestedConfig.Budget
		return task.CompareRequests(&candidate, existing)
	}
	return false
}
