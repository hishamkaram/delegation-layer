package app

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestSilenceClosureSurvivesMissingQueueAnnotation(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsealed", true: "sealed"}[sealed], func(t *testing.T) { checkSilenceClosureOffer(t, sealed) })
	}
}

func checkSilenceClosureOffer(t *testing.T, sealed bool) {
	t.Helper()
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedSilence, launchEnvironment: true, seal: sealed})
	requireBudgetClosureLifetime(t, fixture.td, task.StopCauseOutputSilence)
	records, err := fixture.td.ReadStopRecords()
	if err != nil || len(records) != 0 {
		t.Fatalf("unexpected derived stop annotations: %+v %v", records, err)
	}
	offer := readSilenceClosureOffer(t, fixture)
	if sealed && offer.Resumable {
		t.Fatal("uncollected seal authorized session handoff")
	}
	if !sealed && !offer.Resumable {
		t.Fatalf("closed unsealed turn lost continuation: %+v", offer)
	}
	response := newResponse("status")
	applyContinuationOffer(&response, fixture.root, offer)
	if response.Status != "idle_timed_out" || response.Failure == nil || response.Failure.Code != "task_idle_timed_out" {
		t.Fatalf("response=%+v", response)
	}
	if sealed {
		collectSealedRebootOutcome(t, fixture)
		offer = readSilenceClosureOffer(t, fixture)
		if !offer.Resumable {
			t.Fatalf("collected turn lost continuation: %+v", offer)
		}
	}
}

func readSilenceClosureOffer(t *testing.T, fixture *rebootFixture) *tasklifecycle.ContinuationOffer {
	t.Helper()
	offer, err := tasklifecycle.ObserveTimeoutContinuation(fixture.root, fixture.td, &fixture.req, &fixture.meta, rebootDependencies(t, fixture).Catalog)
	if err != nil || offer == nil || offer.Cause != tasklifecycle.ContinuationIdleTimedOut {
		t.Fatalf("offer=%+v err=%v", offer, err)
	}
	return offer
}
