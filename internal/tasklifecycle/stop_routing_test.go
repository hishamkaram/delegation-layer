package tasklifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

func TestStopRoutingUsesCommittedStopRequest(t *testing.T) {
	store, td, req := lifecyclefixture.NewAppTestTask(t, false)
	t.Cleanup(func() { lifecyclefixture.CloseAppTestTask(t, store, td) })
	lifecyclefixture.PrepareAppBudgetRecords(t, td)
	requestID := strings.Repeat("b", 32)
	refused := errors.New("fixture stops before queue control")
	calls := 0
	opener := func(_ context.Context, root string, binding task.SupervisorRef, source infrastructure.SavedBindingSource, _ pueue.Options, _ supervisorcore.ExecutableLookup) (*pueue.Client, error) {
		calls++
		persisted, err := td.ReadStopRequest(requestID)
		if err != nil {
			t.Fatal(err)
		}
		if root != store.Root || source.RootID != req.RootID || source.OwnerID != td.TaskID || source.RecordID != requestID || source.Kind != infrastructure.SavedBindingTaskStop {
			t.Fatal("stop routing lost exact committed request locator")
		}
		if err = source.ValidateBinding(persisted.Supervisor); err != nil || binding != persisted.Supervisor {
			t.Fatal("stop routing did not use canonical stop binding")
		}
		return nil, refused
	}
	_, err := StopTask(store.Root, td, requestID, "user", pueue.Options{}, supervisorcore.ExecutableLookup{}, opener)
	if !errors.Is(err, refused) || calls != 1 {
		t.Fatalf("stop did not reach source-aware control opener: calls=%d err=%v", calls, err)
	}
}

func TestStopRoutingLeavesLiveTaskWithItsLifetimeOwner(t *testing.T) {
	store, td, _ := lifecyclefixture.NewAppTestTask(t, false)
	t.Cleanup(func() { lifecyclefixture.CloseAppTestTask(t, store, td) })
	lifecyclefixture.PrepareAppBudgetRecords(t, td)
	owner, err := td.TryAcquireLifetimeOwner()
	t.Cleanup(func() {
		if closeErr := owner.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	opener := func(context.Context, string, task.SupervisorRef, infrastructure.SavedBindingSource, pueue.Options, supervisorcore.ExecutableLookup) (*pueue.Client, error) {
		calls++
		return nil, errors.New("live native owner must handle stop")
	}
	stop, err := StopTask(store.Root, td, strings.Repeat("a", 32), "user", pueue.Options{}, supervisorcore.ExecutableLookup{}, opener)
	if err != nil || calls != 0 || !stop.inFlight || !stop.requested {
		t.Fatalf("live task stop entered supervisor routing: calls=%d err=%v", calls, err)
	}
}
