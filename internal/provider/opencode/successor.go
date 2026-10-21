package opencode

import (
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareSuccessorCandidate prepares a new exact-session continuation task
// from its predecessor's immutable record. A predecessor admitted under a
// native run dialect continues through ACP: the successor resumes the same
// recorded session with the recorded environment and writable roots, under
// the ACP launch, capability and current output contracts. An ACP
// predecessor whose recorded predicate is exactly a registered ACP revision
// of its mode keeps its recorded launch preparation, and its successor uses
// the current ACP output contract. Pure-mode predecessors, and ACP records
// with any other predicate, reconstruct exactly like
// PrepareExistingCandidate. The predecessor record is never modified, and no
// path falls back to a new session or a run launch.
func PrepareSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	dialect, err := dialectForRecord(predecessor)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if dialect == dialectACP && isKnownACPReference(request.Mode, predecessor.Predicate) {
		return prepareCandidate(request, dialectACP, &predecessor, acpReferenceForMode(request.Mode), predecessor.RuntimePreflightSHA256 != "")
	}
	if !dialect.nativeRun() {
		return PrepareExistingCandidate(request, predecessor)
	}
	if request.PriorSession == nil {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: an OpenCode successor requires the exact recorded session", ErrUnsupportedProfile)
	}
	return prepareCandidate(request, dialectACP, &predecessor, acpReferenceForMode(request.Mode), true)
}

// isUpgradableOutputReference reports whether reference is one of the exact
// native run revisions that the newest run interpreter corrects.
func isUpgradableOutputReference(mode string, reference task.PredicateRef) bool {
	return reference.Equal(historicalReferenceForMode(mode)) || reference.Equal(historicalCurrentReferenceForMode(mode)) ||
		reference.Equal(historicalCurrentV4ReferenceForMode(mode))
}
