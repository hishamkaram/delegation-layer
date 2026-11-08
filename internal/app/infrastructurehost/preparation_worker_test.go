package infrastructurehost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestInfrastructurePreparationDeadlineRetainsCanonicalResources(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	owner := &infrastructurePreparationOwner{store: store, stores: testStores{}}
	selected := infrastructure.LaunchContext{
		Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS,
		ControlEnvironment: []string{"CONTROL=accepted"}, MaintenanceEnvironment: []string{"MAINTENANCE=accepted"}, SupervisorExecutable: filepath.Join(store.Root, "pueue"),
	}
	ctx, cancel := admissionfixture.NewControllableDeadlineContext(t.Context(), time.Minute)
	defer cancel()
	started, actionDone := make(chan struct{}), make(chan struct{})
	gate, callbackDone := make(chan struct{}), make(chan struct{})
	var release sync.Once
	var result infrastructurePreparationResult
	lateFault := errors.New("canonical action completed with a late fault")
	t.Cleanup(func() {
		release.Do(func() { close(gate) })
		awaitPreparationFixture(t, actionDone)
		awaitPreparationFixture(t, callbackDone)
	})
	go func() {
		defer close(callbackDone)
		result = runInfrastructurePreparation(ctx, selected, func(accepted infrastructure.LaunchContext) infrastructurePreparationResult {
			defer close(actionDone)
			close(started)
			<-gate
			if accepted.ControlEnvironment[0] != "CONTROL=accepted" {
				return infrastructurePreparationResult{err: owner.close(infrastructure.ErrIdentity)}
			}
			return infrastructurePreparationResult{err: owner.close(lateFault)}
		})
	}()
	awaitPreparationFixture(t, started)
	ctx.Expire()
	awaitPreparationFixture(t, callbackDone)
	var retained *infrastructurePreparationWorkerError
	if !errors.Is(result.err, context.DeadlineExceeded) || !errors.As(result.err, &retained) || !retained.CleanupPending() {
		t.Fatalf("expired callback did not retain its actual action: %v", result.err)
	}
	requirePreparationStoreOpen(t, store)
	selected.ControlEnvironment[0] = "CONTROL=caller-mutated"
	release.Do(func() { close(gate) })
	awaitPreparationFixture(t, retained.CleanupDone())
	if retained.CleanupPending() || !errors.Is(retained.CleanupErr(), lateFault) || errors.Is(result.err, lateFault) {
		t.Fatalf("completion lost custody, late fault or immutable callback policy: %v", retained.CleanupErr())
	}
	if _, err := store.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("completed action did not close its owned Store: %v", err)
	}
}

func requirePreparationStoreOpen(t *testing.T, store *taskdir.Store) {
	t.Helper()
	if _, err := store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("callback expiry closed the owned Store: %v", err)
	}
}

func TestInfrastructurePreparationInvalidBootstrapCompletionRetainsParent(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	owner := &infrastructurePreparationOwner{store: store, stores: testStores{}}
	cause := owner.close(&pueue.BootstrapCleanupError{})
	var retained *infrastructurePreparationCleanupError
	if !errors.Is(cause, infrastructure.ErrIdentity) || !errors.As(cause, &retained) || !retained.CleanupPending() {
		t.Fatalf("invalid bootstrap completion released the owned parent: %v", cause)
	}
	if _, err := store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("invalid bootstrap completion closed the Store: %v", err)
	}
	if failure := (Host{}).ClassifyPreparation(cause); !failure.IndependentFault {
		t.Fatal("retained cleanup did not enter resident fail-stop classification")
	}
}

func TestInfrastructurePreparationReleaseFaultStopsAdmissionWithoutHandles(t *testing.T) {
	fault := errors.Join(task.ErrEvidenceFault, errors.New("retired handle release failure"))
	retained := newInfrastructurePreparationCleanupError(&infrastructurePreparationOwner{}, fault)
	awaitPreparationFixture(t, retained.CleanupDone())
	if retained.CleanupPending() || !errors.Is(retained.CleanupErr(), fault) {
		t.Fatal("release failure became usable-handle custody or lost diagnostics")
	}
	if failure := (Host{}).ClassifyPreparation(retained); !failure.IndependentFault {
		t.Fatal("release fault without handles did not stop resident admission")
	}
}

func awaitPreparationFixture(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("owned preparation fixture did not join")
	}
}
