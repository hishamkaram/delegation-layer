package taskdir

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestTryControlObservationRefusesContendedOperationWithoutWaiting(t *testing.T) {
	store := testStore(t)
	id := strings.Repeat("7", 32)
	owner, err := store.OpenInspectionContext(t.Context(), id, true)
	must(t, err)
	t.Cleanup(func() { must(t, owner.Close()) })
	observer, err := store.TryOpenInspectionContext(t.Context(), id)
	must(t, err)
	t.Cleanup(func() { must(t, observer.Close()) })
	before := existingControlTree(t, store.Root)

	must(t, owner.WithLock(t.Context(), func(*ControlTransaction) error {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		called := false
		readErr := observer.TryWithLock(ctx, func(*ControlTransaction) error {
			called = true
			return nil
		})
		if called || !errors.Is(readErr, task.ErrLockBusy) || ctx.Err() != nil {
			t.Fatalf("contended observation: called=%v err=%v context=%v", called, readErr, ctx.Err())
		}
		return nil
	}))
	if after := existingControlTree(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("read observation changed the stored tree")
	}
	must(t, observer.TryWithLock(t.Context(), func(*ControlTransaction) error { return nil }))
}

func TestTryControlOpenMissingCreatesNothing(t *testing.T) {
	store := testStore(t)
	before := existingControlTree(t, store.Root)
	for _, open := range []func(context.Context, string) (*ControlDir, error){store.TryOpenInspectionContext, store.TryOpenWorkspaceOperation} {
		control, err := open(t.Context(), strings.Repeat("9", 32))
		if control != nil || !task.RecordAbsent(err) {
			t.Fatalf("missing observation = (%v, %v)", control, err)
		}
	}
	if after := existingControlTree(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("missing observation created storage")
	}
}

func TestExistingReadObservationRefusesMaintenanceContention(t *testing.T) {
	store := testStore(t)
	must(t, store.maintLock.LockEX(t.Context()))
	defer func() { must(t, store.maintLock.Unlock()) }()
	called := false
	err := store.WithExistingReadObservation(t.Context(), func(context.Context) error {
		called = true
		return nil
	})
	if called || !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("maintenance observation: called=%v err=%v", called, err)
	}
	control, err := store.TryOpenInspectionContext(t.Context(), strings.Repeat("8", 32))
	if control != nil || !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("contended open = (%v, %v)", control, err)
	}
}

func TestExistingReadObservationKeepsNestedCanonicalReaders(t *testing.T) {
	store := testStore(t)
	before := existingControlTree(t, store.Root)
	must(t, store.WithExistingReadObservation(t.Context(), func(ctx context.Context) error {
		ids, err := store.TaskIDs(ctx)
		if err != nil || len(ids) != 0 {
			t.Fatalf("task candidates = (%v, %v)", ids, err)
		}
		return err
	}))
	if after := existingControlTree(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("nested read observation changed storage")
	}
	must(t, store.maintLock.LockEXNonblockingContext(t.Context()))
	must(t, store.maintLock.Unlock())
}

func TestLifetimeTransactionReaderCannotEscapeItsOperation(t *testing.T) {
	_, control, lifetime := newWorkspaceControlLifetime(t, time.Now().Add(-time.Minute))
	_, err := lifetime.SealLifetimeUnlaunchedContext(t.Context())
	must(t, err)
	var escaped ControlLifetimeRecords
	must(t, control.TryWithLock(t.Context(), func(tx *ControlTransaction) error {
		var readErr error
		escaped, readErr = lifetime.TransactionReader(tx)
		if readErr != nil {
			return readErr
		}
		fence, readErr := escaped.ReadLifetimeUnlaunchedContext(t.Context())
		if readErr != nil || fence.TaskID != lifetime.operationID || fence.RootID != lifetime.rootID {
			t.Fatalf("canonical fence = (%+v, %v)", fence, readErr)
		}
		return nil
	}))
	if _, err = escaped.ReadLifetimeUnlaunchedContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("escaped fence reader returned %v", err)
	}
	if _, err = escaped.ReadLifetimeIntentContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("escaped native reader returned %v", err)
	}
	if _, err = lifetime.TransactionReader(nil); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("unbound reader returned %v", err)
	}
}

func TestTaskExecutionObservationReusesOrdinaryClosureAndExclusions(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredNative)
	before := existingControlTree(t, store.Root)
	observed, err := store.ReadTaskExecutionClosureContext(t.Context(), td.TaskID)
	if err != nil || observed.Proof == nil || observed.Proof.Kind != ConnectorClosureRegisteredNative {
		t.Fatalf("ordinary closure = (%+v, %v)", observed, err)
	}
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	defer func() { must(t, owner.Release()) }()
	observed, err = store.ReadTaskExecutionClosureContext(t.Context(), td.TaskID)
	if err != nil || observed.Proof != nil || observed.PendingReason != ConnectorClosureOwnerBusy {
		t.Fatalf("owned ordinary closure = (%+v, %v)", observed, err)
	}
	must(t, owner.Release())
	if after := existingControlTree(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("ordinary closure observation changed task or lock files")
	}
}

func TestTaskPreparationObservationDoesNotCreateClosureOrOwner(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	before := existingControlTree(t, store.Root)
	prepared, err := store.ReadTaskPreparationOnlyContext(t.Context(), td.TaskID)
	if err != nil || !prepared {
		t.Fatalf("preparation = (%v, %v)", prepared, err)
	}
	if after := existingControlTree(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Fatal("preparation observation created evidence or owner inode")
	}
	observed, err := store.ReadTaskExecutionClosureContext(t.Context(), td.TaskID)
	if err != nil || observed.Proof != nil {
		t.Fatalf("preparation became closure = (%+v, %v)", observed, err)
	}
}
