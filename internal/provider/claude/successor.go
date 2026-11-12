package claude

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareSuccessorCandidate prepares a new exact-session continuation task.
// The predecessor's immutable record selects only compatibility: its record
// family, permission mode, predicate eligibility, and static-admission
// eligibility. The caller's environment alone selects the new launch values,
// writable roots, executable, and policy evidence; a predecessor environment
// is never a fallback. A native print predecessor sealed under an exact
// revision that isUpgradableOutputReference lists continues under the current
// predicate for its mode; every other record keeps its recorded predicate. A legacy native OAuth record
// derives a fresh policy proof from the caller's inspection instead of
// retaining the predecessor's. The predecessor record is never modified.
func PrepareSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	if commonprovider.HasNativeProfile(predecessor) {
		return prepareNativeSuccessorCandidate(request, predecessor, environment)
	}
	if hasLegacyNativePolicy(predecessor) {
		return prepareLegacyCandidate(request, predecessor.Predicate, environment)
	}
	return preparePortableCandidate(request, predecessor.Predicate, environment)
}

// prepareNativeSuccessorCandidate keeps the recorded permission mode and
// predicate rules of existing reconstruction and prepares the launch from the
// caller's environment without recorded environment or root overrides, so the
// native builder applies its validation for the retained permission mode.
func prepareNativeSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	options, err := existingNativePreparationOptions(request, predecessor)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if isUpgradableOutputReference(request.Mode, options.predicateReference) {
		options.predicateReference = ReferenceForMode(request.Mode)
	}
	return prepareNativeCandidateWithContract(request, environment, nil, nil, options, predecessor.RuntimePreflightSHA256 != "")
}

// isUpgradableOutputReference reports whether reference is an exact print
// predicate whose interpretation the current predicate for mode corrects: the
// pre-attribution revision, the retained unscoped attribution revision, the
// scoped strict-wire revision, the principal-only advisory revision, and the
// projection revision sealed before failed-request classification, and the
// follow-up revision sealed before authentication diagnosis, each of the same mode.
func isUpgradableOutputReference(mode string, reference task.PredicateRef) bool {
	return reference.Equal(followUpReference(mode)) || reference.Equal(projectionReference(mode)) || reference.Equal(advisoryReference(mode)) || reference.Equal(preAttributionReferenceForMode(mode)) || isUnscopedReference(mode, reference) || reference.Equal(scopedReference(mode))
}
