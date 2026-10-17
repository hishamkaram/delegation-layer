package admission

import (
	"encoding/json"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

type inspectionSubmissionState struct {
	terminalEventRegistrationKnown bool
	terminalEventRegistered        bool
}

type inspectionAwaitState struct {
	identity                       pueue.InspectionIdentity
	polls                          inspectionPollSchedule
	lastStatusErr                  error
	terminalEvents                 bool
	terminalEventSeen              bool
	terminalEventRegistrationKnown bool
	terminalEventRegistered        bool
}

type inspectionAdmissionDecision uint8

const (
	inspectionAdmissionPending inspectionAdmissionDecision = iota
	inspectionAdmissionEligible
	inspectionAdmissionUnavailable
)

type inspectionAdmissionEvaluation struct {
	decision inspectionAdmissionDecision
	facts    json.RawMessage
	recorded bool
}

// inspectionPollSchedule limits targeted queue reads while local proof remains
// observable on every admission tick. Completion can prompt one extra read;
// it never substitutes for an independently observed worker exit.
type inspectionPollSchedule struct {
	nextWatchdog     time.Time
	completionSeen   bool
	callbackExpected bool
}

type completedInspectionAcceptance struct {
	facts json.RawMessage
	ended bool
}

type completedInspectionObservationResult struct {
	facts         json.RawMessage
	acceptedEnded bool
	pending       *pueue.Pending
	completed     *pueue.InspectionObservation
	wait          bool
}

type completedInspectionObservationState struct {
	pending   *pueue.Pending
	completed *pueue.InspectionObservation
}

type completedInspectionObservationStep struct {
	facts json.RawMessage
	done  bool
	wait  bool
}
