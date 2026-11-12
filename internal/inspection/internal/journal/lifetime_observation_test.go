package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestObserveInspectionLifetimeKeepsCanonicalFenceAndScopedReader(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	binding.RunnerOwnership = task.RunnerOwnershipManaged
	binding.WorkerExitProof = WorkerExitProofGuardianWaitV1
	operation, err := OpenOperation(store, request, binding, time.Now().Add(-time.Hour))
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, operation.Close()) })
	_, err = operation.SealLifetimeUnlaunchedContext(t.Context())
	mustInspection(t, err)
	var escaped taskdir.ControlLifetimeRecords
	err = ObserveLifetimeContext(t.Context(), store, request.TaskID, func(observation taskdir.ControlLifetimeObservation) error {
		if !observation.OwnerPresent || observation.PreparationOnly {
			t.Fatalf("fence observation = %+v", observation)
		}
		escaped = observation.Records
		fence, readErr := observation.Records.ReadLifetimeUnlaunchedContext(t.Context())
		if readErr != nil || fence.RootID != store.RootID || fence.TaskID != request.TaskID {
			t.Fatalf("observed canonical fence = (%+v, %v)", fence, readErr)
		}
		return nil
	})
	mustInspection(t, err)
	if _, err = escaped.ReadLifetimeUnlaunchedContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("escaped reader returned %v", err)
	}
}

func TestObserveInspectionLifetimeDoesNotWaitForOwnerOrOperation(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	operation, err := OpenOperation(store, request, binding, time.Now())
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, operation.Close()) })
	owner, err := operation.AcquireLifetimeOwner(t.Context())
	mustInspection(t, err)
	defer func() { mustInspection(t, owner.Release()) }()
	assertBusy := func() {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		called := false
		readErr := ObserveLifetimeContext(ctx, store, request.TaskID, func(taskdir.ControlLifetimeObservation) error {
			called = true
			return nil
		})
		if called || !errors.Is(readErr, task.ErrLockBusy) || ctx.Err() != nil {
			t.Fatalf("busy observation: called=%v err=%v ctx=%v", called, readErr, ctx.Err())
		}
	}
	assertBusy()
	mustInspection(t, owner.Release())
	mustInspection(t, operation.control.WithLock(t.Context(), func(*taskdir.ControlTransaction) error {
		assertBusy()
		return nil
	}))
}

func TestObserveInspectionPreparationKeepsClaimAndRejectsCorruptRequest(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	binding.RunnerOwnership = task.RunnerOwnershipManaged
	binding.WorkerExitProof = WorkerExitProofGuardianWaitV1
	operation, err := OpenOperation(store, request, binding, time.Now())
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, operation.Close()) })
	_, err = operation.ClaimSubmissionContext(t.Context(), time.Now())
	mustInspection(t, err)
	claimPath := filepath.Join(store.Root, "inspections", request.TaskID, submissionRecordName)
	before, err := os.ReadFile(claimPath)
	mustInspection(t, err)
	mustInspection(t, ObserveLifetimeContext(t.Context(), store, request.TaskID, func(observation taskdir.ControlLifetimeObservation) error {
		if !observation.PreparationOnly {
			t.Fatal("request and submission claim were not recognized as preparation")
		}
		return nil
	}))
	after, err := os.ReadFile(claimPath)
	mustInspection(t, err)
	if string(before) != string(after) {
		t.Fatal("observation changed submission claim")
	}
	mustInspection(t, os.WriteFile(filepath.Join(store.Root, "inspections", request.TaskID, requestRecordName), []byte("{"), 0o600))
	called := false
	err = ObserveLifetimeContext(t.Context(), store, request.TaskID, func(taskdir.ControlLifetimeObservation) error {
		called = true
		return nil
	})
	if called || err == nil || task.RecordAbsent(err) {
		t.Fatalf("corrupt request observation: called=%v err=%v", called, err)
	}
}
