package opencode

import (
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareSuccessorCandidate prepares a new exact-session continuation task
// from its predecessor's immutable record. The record decides only
// compatibility: its launch dialect, registered predicate and capability
// eligibility. The caller's environment selects everything else the
// successor launches with: the executable, the filtered launch values, the
// derived writable roots and the capability proof. The predecessor's saved
// environment and roots are never read, and an unusable caller environment
// is refused rather than replaced by them.
//
// A predecessor admitted under a native run dialect continues through ACP:
// the successor resumes the same recorded session under the ACP launch,
// capability and current output contracts. An ACP predecessor whose recorded
// predicate is exactly a registered ACP revision of its mode keeps its
// recorded capability eligibility, and its successor uses the current ACP
// output contract. A pure-mode predecessor keeps the pure-mode profile and its
// recorded predicate, and an ACP record with any other predicate or an unknown
// profile revision is refused exactly like PrepareExistingCandidate. The
// predecessor record is never modified, and no path falls back to a new
// session or a run launch.
func PrepareSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	dialect, err := dialectForRecord(predecessor)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if dialect == dialectACP && isKnownACPReference(request.Mode, predecessor.Predicate) {
		return prepareCandidate(request, environment, dialectACP, nil, acpReferenceForMode(request.Mode), predecessor.RuntimePreflightSHA256 != "")
	}
	if dialect.nativeRun() {
		if request.PriorSession == nil {
			return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: an OpenCode successor requires the exact recorded session", ErrUnsupportedProfile)
		}
		return prepareCandidate(request, environment, dialectACP, nil, acpReferenceForMode(request.Mode), true)
	}
	reference, err := dialect.recordedPredicate(request.Mode, predecessor.Predicate)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return prepareCandidate(request, environment, dialect, nil, reference, false)
}

// isUpgradableOutputReference reports whether reference is one of the exact
// native run revisions that the newest run interpreter corrects.
func isUpgradableOutputReference(mode string, reference task.PredicateRef) bool {
	return reference.Equal(historicalReferenceForMode(mode)) || reference.Equal(historicalCurrentReferenceForMode(mode)) ||
		reference.Equal(historicalCurrentV4ReferenceForMode(mode))
}
