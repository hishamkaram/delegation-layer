package antigravity

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareSuccessorCandidate prepares a new exact-session continuation task
// from its predecessor's immutable record. The predecessor decides only what
// the continuation may be: its native or portable profile, its recorded
// approval, its static-admission policy, and its output predicate. The caller's
// current environment decides the launch: filtered values, derived roots,
// executable, and capability proof are those of a new task admitted there, and
// an invalid current environment fails instead of falling back to the saved
// one. A portable predecessor is reconstructed as PrepareExistingCandidate does.
// A native predecessor sealed under the auth2, auth3, or print-record-v1
// revision continues under the current record revision; the unproven base
// revision, portable records, and every other record keep the recorded
// predicate. The predecessor record is never modified.
func PrepareSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	if !commonprovider.HasNativeProfile(predecessor) {
		return PrepareExistingCandidate(request, predecessor, environment)
	}
	predicate := predecessor.Predicate
	if isUpgradableOutputReference(request.Mode, predicate) {
		predicate = NewRecordPrintInterpreter(request.Mode).Reference()
	}
	candidate, err := prepareSuccessorProfile(request, predecessor, environment)
	if err != nil {
		return candidate, err
	}
	return withRecordedPredicate(candidate, predicate), nil
}

// isUpgradableOutputReference reports whether reference is one of the exact
// envelope or record revisions the current record interpreter corrects.
func isUpgradableOutputReference(mode string, reference task.PredicateRef) bool {
	return reference.Equal(newHistoricalCurrentPrintInterpreterForMode(mode).Reference()) ||
		reference.Equal(NewCurrentPrintInterpreterForMode(mode).Reference()) ||
		reference.Equal(newHistoricalRecordPrintInterpreter(mode).Reference())
}
