package tasklifecycle

import "github.com/hishamkaram/delegation-layer/internal/taskdir"

type CancellationFailure uint8

const (
	CancellationFailureGeneral CancellationFailure = iota
	CancellationFailureSubmissionRead
)

// Cancellation contains detached observations returned to the caller. These
// values cannot authorize work; taskdir's journals retain that authority.
type Cancellation struct {
	Inspection *taskdir.TaskInspection
	Stop       *StopOutcome
	Stops      []taskdir.StopRecord
	StopsRead  bool
	Failure    CancellationFailure
}

type RecheckStage uint8

const (
	RecheckInspection RecheckStage = iota
	RecheckEligibility
	RecheckInterpreter
	RecheckAssessment
)

// Reassessment preserves the original publication beside its separate offline
// assessment. Stage identifies where an error occurred, without CLI codes.
type Reassessment struct {
	Inspection *taskdir.TaskInspection
	Assessment *taskdir.RecheckResult
	Stage      RecheckStage
}
