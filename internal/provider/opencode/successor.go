package opencode

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareSuccessorCandidate prepares a new exact-session continuation task
// from its predecessor's immutable record. It reconstructs the recorded launch
// and environment like PrepareExistingCandidate, and changes only the output
// predicate: a native predecessor sealed under revision 2, 3, or 4 continues
// under the current revision. Legacy and every other record keep the recorded
// predicate. The predecessor record is never modified.
func PrepareSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	if commonprovider.HasNativeProfile(predecessor) && isUpgradableOutputReference(request.Mode, predecessor.Predicate) {
		predecessor.Predicate = ReferenceForMode(request.Mode)
	}
	return PrepareExistingCandidate(request, predecessor)
}

// isUpgradableOutputReference reports whether reference is one of the exact
// native revisions the current interpreter corrects.
func isUpgradableOutputReference(mode string, reference task.PredicateRef) bool {
	return reference.Equal(historicalReferenceForMode(mode)) || reference.Equal(historicalCurrentReferenceForMode(mode)) ||
		reference.Equal(historicalCurrentV4ReferenceForMode(mode))
}
