package antigravity

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareSuccessorCandidate prepares a new exact-session continuation task
// from its predecessor's immutable record. It reconstructs the recorded launch,
// approval, environment, roots, and probes like PrepareExistingCandidate, and
// changes only the output predicate: a native predecessor sealed under the
// auth2, auth3, or print-record-v1 revision continues under the current record
// revision. The unproven base revision, portable records, and every other
// record keep the recorded predicate. The predecessor record is never modified.
func PrepareSuccessorCandidate(request task.TaskRecord, predecessor task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	if commonprovider.HasNativeProfile(predecessor) && isUpgradableOutputReference(request.Mode, predecessor.Predicate) {
		predecessor.Predicate = NewRecordPrintInterpreter(request.Mode).Reference()
	}
	return PrepareExistingCandidate(request, predecessor)
}

// isUpgradableOutputReference reports whether reference is one of the exact
// envelope or record revisions the current record interpreter corrects.
func isUpgradableOutputReference(mode string, reference task.PredicateRef) bool {
	return reference.Equal(newHistoricalCurrentPrintInterpreterForMode(mode).Reference()) ||
		reference.Equal(NewCurrentPrintInterpreterForMode(mode).Reference()) ||
		reference.Equal(newHistoricalRecordPrintInterpreter(mode).Reference())
}
