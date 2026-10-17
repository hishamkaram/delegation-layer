package tasklifecycle

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

type pendingStopLifetimeOwner struct {
	pending <-chan struct{}
	entered chan struct{}
}

func (owner *pendingStopLifetimeOwner) Release() error {
	select {
	case <-owner.pending:
	default:
		return errors.New("lifetime fence released before the supervisor stop settled")
	}
	close(owner.entered)
	return nil
}

func TestReleaseLifetimeOwnerWaitsForPendingSupervisorStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pending := make(chan struct{})
		owner := &pendingStopLifetimeOwner{pending: pending, entered: make(chan struct{})}
		done := make(chan error, 1)
		go func() {
			done <- releaseLifetimeOwnerAfterPending(pending, owner)
		}()

		pendingClosed := false
		defer func() {
			if !pendingClosed {
				close(pending)
			}
			synctest.Wait()
		}()

		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("lifetime owner release returned before stop settlement: %v", err)
		case <-owner.entered:
			t.Fatal("lifetime owner release entered before stop settlement")
		default:
		}

		close(pending)
		pendingClosed = true
		synctest.Wait()
		select {
		case <-owner.entered:
		default:
			t.Fatal("lifetime owner release did not enter after stop settlement")
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

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
			store, td, _ := lifecyclefixture.NewAppTestTask(t, false)
			defer lifecyclefixture.CloseAppTestTask(t, store, td)
			lifecyclefixture.PrepareAppBudgetRecords(t, td)
			if tc.registerLifetime {
				lifecyclefixture.RecordRecoverableLifetime(t, td, admissionfixture.CurrentBootID(t))
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
