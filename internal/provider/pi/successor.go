package pi

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareSuccessorCandidate prepares a new exact-session continuation task
// from its predecessor's immutable record. It reconstructs the recorded
// launch, environment, roots, and policy like PrepareExistingCandidate and
// changes only the output predicate: a predecessor admitted under the prior
// fresh-default revision of its mode shares the current launch contract, so
// its successor is interpreted by the current native-outcome policy. Every
// other record keeps its recorded predicate and launch. The predecessor
// record is never modified.
func PrepareSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	if commonprovider.HasNativeProfile(predecessor) && predecessor.Predicate.Equal(priorReferenceForMode(request.Mode)) {
		predecessor.Predicate = ReferenceForMode(request.Mode)
	}
	return PrepareExistingCandidate(request, predecessor, environment)
}
