package runtime

import (
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/recurrence"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type tickDecision struct {
	due  time.Time
	skip *contract.SkipSpan
	hold bool
}

type tickInput struct {
	summary            contract.ScheduleSummary
	startup            time.Time
	now                time.Time
	held               time.Time
	active             *contract.Occurrence
	admissionAvailable bool
}

// decideTick uses the recurrence owner's calendar semantics. A compact skip
// advances straight to the watermark; it never enumerates historical minutes.
func decideTick(input tickInput) (tickDecision, error) {
	summary := input.summary
	if summary.State != contract.StateEnabled {
		return tickDecision{}, nil
	}
	due, err := recurrence.Next(summary.Header.Cron, summary.Header.Timezone, summary.Cursor)
	if err != nil {
		return tickDecision{}, err
	}
	if !due.After(input.startup) {
		return skippedTick(summary.Cursor, input.startup, contract.SkipMissedUnavailable), nil
	}
	if due.After(input.now) {
		return tickDecision{}, nil
	}
	if !input.now.Before(due.Add(ClaimWindow)) {
		if due.Equal(input.held) {
			if input.active == nil && !input.admissionAvailable {
				return skippedTick(summary.Cursor, due, contract.SkipMissedCapacity), nil
			}
			return skippedTick(summary.Cursor, due, blockerReason(input.active)), nil
		}
		return skippedTick(summary.Cursor, input.now.Add(-ClaimWindow), contract.SkipMissedUnavailable), nil
	}
	if input.active == nil && !input.admissionAvailable {
		return skippedTick(summary.Cursor, due, contract.SkipMissedCapacity), nil
	}
	return tickDecision{due: due, hold: input.active != nil}, nil
}

func skippedTick(from, to time.Time, reason contract.SkipReason) tickDecision {
	return tickDecision{skip: &contract.SkipSpan{From: from, To: to, Reason: reason}}
}

func blockerReason(active *contract.Occurrence) contract.SkipReason {
	if active == nil {
		return contract.SkipMissedUnavailable
	}
	if active.Observation != nil && active.Observation.Admission == task.AdmissionAdmitted {
		return contract.SkipOverlapActive
	}
	return contract.SkipBlockedUncertain
}

func sameOccurrence(left, right contract.Occurrence) bool {
	return left.TaskID == right.TaskID && left.ScheduleID == right.ScheduleID &&
		left.Due.Equal(right.Due) && left.ArmedAt.Equal(right.ArmedAt)
}

func claimOccurrence(claim contract.Claim) contract.Occurrence {
	return contract.Occurrence{TaskID: claim.TaskID, ScheduleID: claim.ScheduleID, Due: claim.Due, ArmedAt: claim.ArmedAt}
}
