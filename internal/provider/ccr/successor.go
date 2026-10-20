package ccr

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareSuccessorCandidate prepares a new exact-session continuation task
// from its predecessor's immutable record. It reconstructs the recorded launch
// shape (the retained CCR route or the explicit model and provider-only auth),
// environment, roots, and capability inspection like PrepareExistingCandidate,
// and changes only the output predicate: a credential-aware v4 or v5
// predecessor continues under the current v6 predicate of the same launch
// shape. Every other record keeps its recorded predicate. The predecessor
// record is never modified.
func PrepareSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	if target, ok := currentOutputReference(request.Mode, predecessor.Predicate); ok {
		predecessor.Predicate = target
	}
	return PrepareExistingCandidate(request, predecessor)
}

// currentOutputReference maps an exact v4 or v5 reference to the current
// reference that shares its route-preserving or modeled launch shape.
func currentOutputReference(mode string, source task.PredicateRef) (task.PredicateRef, bool) {
	switch {
	case source.Equal(preAttributionReference(mode, true)), source.Equal(scopedReference(mode, true)):
		return Reference(mode), true
	case source.Equal(preAttributionReference(mode, false)), source.Equal(scopedReference(mode, false)):
		return modeledReference(mode), true
	default:
		return task.PredicateRef{}, false
	}
}
