package claude

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareSuccessorCandidate prepares a new exact-session continuation task
// from its predecessor's immutable record. It reconstructs the recorded
// launch, environment, roots, and policy like PrepareExistingCandidate, and
// changes only the output predicate: a native print predecessor sealed before
// scoped completion attribution continues under the current predicate for its
// mode. Every other record, including interactive and portable ones, keeps the
// recorded predicate. The predecessor record is never modified.
func PrepareSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	if commonprovider.HasNativeProfile(predecessor) && isUpgradableOutputReference(request.Mode, predecessor.Predicate) {
		predecessor.Predicate = ReferenceForMode(request.Mode)
	}
	return PrepareExistingCandidate(request, predecessor)
}

// isUpgradableOutputReference reports whether reference is an exact print
// predicate whose interpretation of follow-up completions the current
// predicate for mode corrects: the pre-attribution revision and the retained
// unscoped attribution revision of the same mode.
func isUpgradableOutputReference(mode string, reference task.PredicateRef) bool {
	return reference.Equal(preAttributionReferenceForMode(mode)) || isUnscopedReference(mode, reference)
}
