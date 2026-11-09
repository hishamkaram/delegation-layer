package tasklifecycle

import (
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestOutputSilenceContinuationInheritsOverridesAndDisables(t *testing.T) {
	predecessor := task.TaskConfig{OutputSilenceTimeout: "30s", Interactive: true, Permission: "read-only"}
	for _, tc := range []struct{ value, want string }{{"", "30s"}, {"250ms", "250ms"}, {"0s", "0s"}} {
		got := ContinuationConfig(predecessor, task.TaskConfig{OutputSilenceTimeout: tc.value})
		if got.OutputSilenceTimeout != tc.want || !got.Interactive || got.Permission != "read-only" {
			t.Fatalf("override %q => %+v", tc.value, got)
		}
	}
	if predecessor.OutputSilenceTimeout != "30s" {
		t.Fatal("predecessor mutated")
	}
}

func TestSilenceStopIsTimeoutWithDistinctContinuationCause(t *testing.T) {
	req := &task.TaskRecord{Provider: "claude:print", TaskID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	records := []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: task.StopCauseOutputSilence}, Observation: &task.StopObservedRecord{Terminated: true}}}
	offer := TimeoutContinuation(req, records, commonprovider.Catalog{}, ContinuationSessionObserved, "/installed/delegate-run", "", nil, true, true)
	if offer == nil || offer.Cause != ContinuationIdleTimedOut {
		t.Fatalf("offer=%+v", offer)
	}
	records[0].Observation.Terminated = false
	if offer = TimeoutContinuation(req, records, commonprovider.Catalog{}, ContinuationSessionObserved, "/installed/delegate-run", "", nil, true, true); offer != nil {
		t.Fatalf("unfinished stop authorized offer: %+v", offer)
	}
}
