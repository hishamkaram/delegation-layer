package journal

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestObserveWorkspaceLifetimeReusesCanonicalFenceAndRejectsEscape(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('7'), time.Now().Add(-time.Minute)))
	_, err := operation.SealLifetimeUnlaunchedContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var escaped taskdir.ControlLifetimeRecords
	err = ObserveLifetimeContext(t.Context(), store, operation.Request().OperationID, func(observation taskdir.ControlLifetimeObservation) error {
		if !observation.OwnerPresent || observation.PreparationOnly {
			t.Fatalf("fence observation = %+v", observation)
		}
		escaped = observation.Records
		fence, readErr := observation.Records.ReadLifetimeUnlaunchedContext(t.Context())
		if readErr != nil || fence.RootID != store.RootID || fence.TaskID != operation.Request().OperationID {
			t.Fatalf("workspace fence = (%+v, %v)", fence, readErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = escaped.ReadLifetimeUnlaunchedContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("escaped reader returned %v", err)
	}
}

func TestObserveWorkspaceLifetimeDoesNotWaitAndPreservesPreparation(t *testing.T) {
	store := newCheckpointStore(t)
	operation := openCheckpointOperation(t, store, ordinaryCheckpointRequest(store, testID('8'), time.Now().Add(time.Minute)))
	_, err := operation.SubmissionPermit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = ObserveLifetimeContext(t.Context(), store, operation.Request().OperationID, func(observation taskdir.ControlLifetimeObservation) error {
		if !observation.PreparationOnly {
			t.Fatal("request and submission guard were not preparation-only")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := operation.TryAcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := owner.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	called := false
	err = ObserveLifetimeContext(ctx, store, operation.Request().OperationID, func(taskdir.ControlLifetimeObservation) error {
		called = true
		return nil
	})
	if called || !errors.Is(err, task.ErrLockBusy) || ctx.Err() != nil {
		t.Fatalf("owned observation: called=%v err=%v ctx=%v", called, err, ctx.Err())
	}
}
