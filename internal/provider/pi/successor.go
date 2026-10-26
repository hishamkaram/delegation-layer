package pi

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareSuccessorCandidate prepares a new exact-session continuation task.
// The predecessor's immutable record selects only compatibility: the launch
// profile, the recorded output predicate, and static-admission eligibility.
// The caller's environment is the successor's launch environment, so its
// filtered values, derived roots, session directory, capability proof, and
// executable lookup never inherit the predecessor's saved ones; an invalid
// caller environment fails rather than falling back to the record. A
// predecessor admitted under the prior fresh-default revision of its mode
// shares the current launch contract, so its successor is interpreted by the
// current native-outcome policy. Every other record keeps its recorded
// predicate and launch profile. The predecessor record is never modified.
func PrepareSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	if commonprovider.HasNativeProfile(predecessor) && predecessor.Predicate.Equal(priorReferenceForMode(request.Mode)) {
		predecessor.Predicate = ReferenceForMode(request.Mode)
	}
	profile := recordedProfile(predecessor)
	return prepareCandidate(request, profile, predecessor.Predicate, environment, environment, retainsStaticAdmission(predecessor, profile))
}
