package ccr

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareSuccessorCandidate prepares a new exact-session continuation task.
// The predecessor's immutable record selects only compatibility: its
// recorded-environment eligibility and its predicate, which fixes the launch
// shape (the retained CCR route or the explicit model and provider-only auth)
// and the inspection contract. The caller's environment alone selects the
// executable, the filtered launch values, the writable roots, and the runtime
// inspection; a predecessor environment or root is never a fallback. A
// credential-aware v4, v5, or v6 predecessor continues under the current v7
// predicate of the same launch shape; every other record keeps its recorded
// predicate. The predecessor record is never modified.
func PrepareSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	if !predecessor.EnvironmentRecorded {
		return commonprovider.ProfileCandidate{}, task.ErrIdentityMismatch
	}
	reference := predecessor.Predicate
	if target, ok := currentOutputReference(request.Mode, reference); ok {
		reference = target
	}
	return prepare(request, environment, environment, nil, reference)
}

// currentOutputReference maps an exact v4 or v5 reference to the current
// reference that shares its route-preserving or modeled launch shape.
func currentOutputReference(mode string, source task.PredicateRef) (task.PredicateRef, bool) {
	switch {
	case source.Equal(advisoryReference(mode, true)), source.Equal(preAttributionReference(mode, true)), source.Equal(scopedReference(mode, true)):
		return Reference(mode), true
	case source.Equal(advisoryReference(mode, false)), source.Equal(preAttributionReference(mode, false)), source.Equal(scopedReference(mode, false)):
		return modeledReference(mode), true
	default:
		return task.PredicateRef{}, false
	}
}
