package execution

import (
	"bytes"
	"errors"
	"os/exec"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPreflightRefusalSealsStartFailedWithoutLaunching(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "echo")
	events := &eventLog{}
	injected := errors.New("compiled profile changed before launch")
	preflightCalls, startCalls := 0, 0

	result := Run(td, permit, plan, Options{
		Preflight: func() error {
			preflightCalls++
			events.add("preflight")
			return injected
		},
		Hooks: Hooks{
			Event: events.add,
			Start: func(*exec.Cmd) error {
				startCalls++
				events.add("start-hook")
				return errors.New("unexpected provider launch")
			},
		},
	})
	require(t, errors.Join(result.Error, result.CleanupError, result.ReceiptError, result.StopError))
	if result.Outcome == nil || result.Outcome.Verdict != task.VerdictRejected {
		t.Fatalf("preflight refusal was not published as a rejection: %+v", result)
	}
	if preflightCalls != 1 || startCalls != 0 {
		t.Fatalf("preflight=%d start=%d, want one preflight and no launch", preflightCalls, startCalls)
	}
	if got := string(payload(t, td, result.Outcome)); got != "start_failed: "+injected.Error() {
		t.Fatalf("wrong refusal payload: %q", got)
	}
	inspection, err := td.Inspect()
	require(t, err)
	if !inspection.StartExists || !inspection.SealExists || !inspection.OutcomeExists || inspection.StartedExists {
		t.Fatalf("preflight refusal left incomplete authority: %+v", inspection)
	}
	eventsSnapshot := events.snapshot()
	assertOrder(t, eventsSnapshot, "start-entry", "preflight")
	assertOrder(t, eventsSnapshot, "preflight", "start-failed")

	replayed, cleanupErr, collectErr := td.Collect(plan.Predicate)
	require(t, errors.Join(cleanupErr, collectErr))
	if replayed == nil || replayed.Verdict != result.Outcome.Verdict {
		t.Fatalf("sealed refusal did not replay: %+v", replayed)
	}
	if !bytes.Equal(payload(t, td, replayed), payload(t, td, result.Outcome)) {
		t.Fatal("replayed refusal payload changed")
	}
}

func TestPreflightPassesBeforeExactlyOneLaunch(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "echo")
	events := &eventLog{}
	preflightCalls, startCalls := 0, 0

	result := Run(td, permit, plan, Options{
		Preflight: func() error {
			preflightCalls++
			events.add("preflight")
			return nil
		},
		Hooks: Hooks{
			Event: events.add,
			Start: func(cmd *exec.Cmd) error {
				startCalls++
				events.add("start-hook")
				return cmd.Start()
			},
		},
	})
	assertSuccess(t, result)
	if preflightCalls != 1 || startCalls != 1 {
		t.Fatalf("preflight=%d start=%d, want one each", preflightCalls, startCalls)
	}
	eventsSnapshot := events.snapshot()
	assertOrder(t, eventsSnapshot, "start-entry", "preflight")
	assertOrder(t, eventsSnapshot, "preflight", "start-hook")
	assertOrder(t, eventsSnapshot, "start-hook", "started")
}
