package execution

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOuterDeadlineCannotExtendBudgetOrSkipFinalization(t *testing.T) {
	td, permit, plan, _ := fixtureTaskWithPlan(t, "echo", configurePendingRuntimePreflight)
	request, _, err := td.PreparedRecords()
	require(t, err)
	clock := newAdvancingClock()
	startCalls, preflightCalls := 0, 0
	result := Run(td, permit, plan, Options{
		Clock: clock, Deadline: clock.Now().Add(time.Duration(request.BudgetNanos) + time.Second),
		PreflightPlan: func(PreflightScope, Plan) (Plan, error) {
			preflightCalls++
			return Plan{}, errors.New("unexpected preflight")
		},
		Hooks: Hooks{Start: func(*exec.Cmd) error {
			startCalls++
			return errors.New("unexpected provider launch")
		}},
	})
	assertPendingRuntimeRejected(t, td, result)
	if preflightCalls != 0 || startCalls != 0 {
		t.Fatalf("invalid deadline reached preflight=%d or Start=%d", preflightCalls, startCalls)
	}
	if !strings.Contains(string(payload(t, td, result.Outcome)), task.ErrIdentityMismatch.Error()) {
		t.Fatalf("invalid deadline did not seal its identity refusal: %+v", result.Outcome)
	}
}
