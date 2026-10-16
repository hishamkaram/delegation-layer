package app

import (
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRecordStoppedTaskTerminationRequiresNativeLifetimeProof(t *testing.T) {
	const requestID = "cccccccccccccccccccccccccccccccc"
	for _, tc := range []struct {
		name             string
		registerLifetime bool
		wantTerminated   bool
		wantUnverifiable bool
		wantObservation  bool
	}{
		{
			name:            "historical task without lifetime registration",
			wantTerminated:  true,
			wantObservation: true,
		},
		{
			name:             "registered cooperative scope without launch identity",
			registerLifetime: true,
			wantUnverifiable: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, td, _ := newAppTestTask(t, false)
			defer closeAppTestTask(t, store, td)
			prepareAppBudgetRecords(t, td)
			if tc.registerLifetime {
				recordRecoverableLifetime(t, td, currentBootID(t))
			}
			prepareTerminationStopRecord(t, td, requestID)

			id := int64(7)
			terminated, err := recordStoppedTaskTermination(t.Context(), td, requestID, pueue.StopResult{
				Matched:       true,
				ObservedState: pueue.StateEnded,
				NumericTaskID: &id,
			})
			if terminated != tc.wantTerminated {
				t.Fatalf("terminated = %t, want %t", terminated, tc.wantTerminated)
			}
			if tc.wantUnverifiable != errors.Is(err, lifetime.ErrUnverifiableLaunch) {
				t.Fatalf("error = %v, want unverifiable=%t", err, tc.wantUnverifiable)
			}

			records, readErr := td.ReadStopRecords()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(records) != 1 {
				t.Fatalf("stop records = %d, want 1", len(records))
			}
			observed := records[0].Observation != nil && records[0].Observation.Terminated
			if observed != tc.wantObservation {
				t.Fatalf("termination observation = %t, want %t: %+v", observed, tc.wantObservation, records[0])
			}
		})
	}
}

func prepareTerminationStopRecord(t *testing.T, td *taskdir.TaskDir, requestID string) {
	t.Helper()
	permit, err := td.PrepareStop(requestID, "user", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStopReply(requestID, task.StopReplyFacts{
		NumericTaskID: 7,
		Action:        "kill",
		Acknowledged:  true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCancelPreparedTaskNeedsNoSupervisorOrNativeFacts(t *testing.T) {
	store, td, request := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	// The fixture's supervisor executable does not exist. A successful cancel
	// therefore proves this unsubmitted path never needed a supervisor client.
	result := cancel(Arguments{Root: store.Root, TaskID: request.TaskID}, Dependencies{})
	if result.code != 0 || result.response.Stop == nil {
		t.Fatalf("prepared cancellation: code=%d response=%+v", result.code, result.response)
	}
	stop := result.response.Stop
	if stop.NumericTaskID != nil || stop.Action != "not-launched" || !stop.Terminated {
		t.Fatalf("unsubmitted cancellation fabricated queue activity: %+v", stop)
	}
	if result.response.Outcome != nil {
		t.Fatal("unsubmitted cancellation fabricated a provider outcome")
	}
	records, err := td.ReadStopRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Request == nil || records[0].Request.Cause != "user" || records[0].Observation != nil {
		t.Fatalf("incorrect durable cancellation evidence: %+v", records)
	}
	assertTaskHasNoNativeLifetimeEvidence(t, td)
}

func TestCancelSubmittedTaskWithoutReceiptNeedsNoSupervisor(t *testing.T) {
	store, td, request := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	permit, err := td.PrepareSubmission(meta.SupervisorConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	// Submission has committed but no numeric receipt has been published.
	// The nonexistent saved supervisor binary must never be invoked to cancel.
	result := cancel(Arguments{Root: store.Root, TaskID: request.TaskID}, Dependencies{})
	if result.code != 0 || result.response.Stop == nil || !result.response.Stop.Requested || result.response.Stop.NumericTaskID != nil {
		t.Fatalf("receipt-less cancellation tried to control a supervisor row: %+v", result)
	}
	if result.response.Outcome != nil {
		t.Fatal("receipt-less cancellation fabricated provider publication")
	}
	assertTaskHasNoNativeLifetimeEvidence(t, td)
}

func assertTaskHasNoNativeLifetimeEvidence(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	evidence, err := taskdir.LoadLifetimeEvidenceContext(t.Context(), td)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Intent != nil || evidence.Registration != nil || evidence.WorkerWait != nil || evidence.Termination != nil {
		t.Fatal("cancellation fabricated native lifetime evidence")
	}
}
