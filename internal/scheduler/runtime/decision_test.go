package runtime

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/recurrence"
)

func TestSchedulerDecisionUsesExistingRecurrenceSemantics(t *testing.T) {
	for _, test := range []struct{ cron, zone, after, due string }{
		{"0 0 15 * MON", "UTC", "2026-06-01T00:00:00Z", "2026-06-08T00:00:00Z"},
		{"5/20 * * * *", "UTC", "2026-01-01T00:06:00Z", "2026-01-01T00:25:00Z"},
		{"30 2 * * *", "Europe/Berlin", "2026-03-28T01:30:00Z", "2026-03-30T00:30:00Z"},
		{"30 2 * * *", "Europe/Berlin", "2026-10-25T00:30:00Z", "2026-10-26T01:30:00Z"},
	} {
		t.Run(test.after+test.zone, func(t *testing.T) {
			summary := contract.ScheduleSummary{
				Header: contract.DefinitionHeader{Cron: test.cron, Timezone: test.zone},
				State:  contract.StateEnabled, Cursor: instant(test.after),
			}
			input := tickInput{summary: summary, startup: summary.Cursor, now: instant(test.due), admissionAvailable: true}
			decision, err := decideTick(input)
			if err != nil || !decision.due.Equal(instant(test.due)) || decision.skip != nil {
				t.Fatalf("decision=%+v error=%v", decision, err)
			}
		})
	}
}

func TestSchedulerDecisionWindowStartupAndReleasedCapacity(t *testing.T) {
	created := instant("2026-01-01T00:00:00Z")
	due := created.Add(time.Minute)
	base := tickInput{summary: contract.ScheduleSummary{
		Header: contract.DefinitionHeader{Cron: "* * * * *", Timezone: "UTC"},
		State:  contract.StateEnabled, Cursor: created,
	}, startup: created.Add(30 * time.Second), admissionAvailable: true}
	for _, offset := range []time.Duration{0, ClaimWindow - time.Nanosecond, ClaimWindow} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			input := base
			input.now = due.Add(offset)
			decision, err := decideTick(input)
			if err != nil {
				t.Fatal(err)
			}
			if offset < ClaimWindow {
				if !decision.due.Equal(due) || decision.skip != nil {
					t.Fatalf("eligible boundary: %+v", decision)
				}
			} else if decision.skip == nil || decision.skip.Reason != contract.SkipMissedUnavailable {
				t.Fatalf("expiry boundary: %+v", decision)
			}
		})
	}
	base.now, base.held, base.admissionAvailable = due.Add(ClaimWindow), due, false
	decision, err := decideTick(base)
	if err != nil || decision.skip == nil || decision.skip.Reason != contract.SkipMissedCapacity {
		t.Fatalf("released at expiry with full capacity: %+v %v", decision, err)
	}
	base.startup, base.now = due, due
	decision, err = decideTick(base)
	if err != nil || decision.skip == nil || decision.skip.Reason != contract.SkipMissedUnavailable {
		t.Fatalf("startup watermark must win: %+v %v", decision, err)
	}
}

func TestSchedulerDecisionRangeExhaustionAndFaultsRemainExplicit(t *testing.T) {
	input := tickInput{summary: contract.ScheduleSummary{
		Header: contract.DefinitionHeader{Cron: "* * * * *", Timezone: "UTC"},
		Cursor: instant("2262-04-11T23:47:00Z"), State: contract.StateEnabled,
	}}
	if _, err := decideTick(input); !errors.Is(err, recurrence.ErrRangeExhausted) {
		t.Fatalf("range exhaustion=%v", err)
	}
	input.summary.Header.Cron = "invalid"
	if _, err := decideTick(input); !errors.Is(err, recurrence.ErrInvalidExpression) {
		t.Fatalf("invalid recurrence=%v", err)
	}
	fault := errors.New("disk fault")
	if cancellationOnly(errors.Join(fmt.Errorf("wrapped: %w", errors.New("other")), fault)) {
		t.Fatal("fault mistaken for cancellation")
	}
}
