package claude

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareSuccessorCandidate prepares a new exact-session continuation task
// from its predecessor's immutable record. It reconstructs the recorded
// launch, environment, roots, and policy like PrepareExistingCandidate, and
// changes only the output predicate: a native print predecessor sealed before
// the advisory wire revision continues under the current predicate for its
// mode. Every other record, including interactive and portable ones, keeps the
// recorded predicate. The predecessor record is never modified.
func PrepareSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	if commonprovider.HasNativeProfile(predecessor) && isUpgradableOutputReference(request.Mode, predecessor.Predicate) {
		predecessor.Predicate = ReferenceForMode(request.Mode)
	}
	return PrepareExistingCandidate(request, predecessor, environment)
}

// isUpgradableOutputReference reports whether reference is an exact print
// predicate whose interpretation the current predicate for mode corrects: the
// pre-attribution revision, the retained unscoped attribution revision, and
// the scoped strict-wire revision of the same mode.
func isUpgradableOutputReference(mode string, reference task.PredicateRef) bool {
	return reference.Equal(preAttributionReferenceForMode(mode)) || isUnscopedReference(mode, reference) || reference.Equal(scopedReference(mode))
}
