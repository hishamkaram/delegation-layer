package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

// newPlacedTaskInPool records a lifetime of a new private task with the given
// placement, closed by cause unless cause is custodyLifetimeOpen, and binds one
// view for its owner. Tasks sharing a placement share one real pool, so their
// releases share its images.
func newPlacedTaskInPool(t *testing.T, store *taskdir.Store, id string, placement *task.LifetimeCustodyPlacement, cause string) *task.LifetimeRegistrationRecord {
	t.Helper()
	td, _ := lifecyclefixture.NewAppTaskInStoreWithSupervisorConfig(t, store, id, false, nil, pueue.PrivateConfigPath(store.Root))
	t.Cleanup(func() {
		if err := td.Close(); err != nil {
			t.Error(err)
		}
	})
	registration := recordCustodyLifetime(t, td, placement, cause)
	bindCustodyTestView(t, registration)
	return registration
}

// openDescriptorCount counts this process's open file descriptors through the
// descriptor directory both Linux and macOS provide. The listing's own
// descriptor is counted the same way on every call.
func openDescriptorCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func custodyTestTaskID(digit string) string { return strings.Repeat(digit, 32) }

// TestFullCustodyPassReleasesClosedTasksConcurrentlyOverSharedStore composes
// the elected owner's full custody phase over one shared store, the canonical
// guardian closure proof and real pools. Five closed tasks share one pool with
// an open lifetime, one closed task has its own pool, and two closed tasks
// interleaved in the inventory have missing pools; an unplaced task and a
// partially created directory hold nothing. One pass releases every closed
// task whose pool is present, keeps the open lifetime's custody in the shared
// pool, reports both missing pools in inventory order without skipping a
// sibling, and closes every task handle and pool descriptor it opened. Once the
// pools are back, the next pass releases them and still keeps the open one.
func TestFullCustodyPassReleasesClosedTasksConcurrentlyOverSharedStore(t *testing.T) {
	store, unplaced, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, unplaced)
	shared := newCustodyTestPlacement(t)
	var closed []*task.LifetimeRegistrationRecord
	for _, digit := range []string{"2", "4", "5", "7", "9"} {
		closed = append(closed, newPlacedTaskInPool(t, store, custodyTestTaskID(digit), shared, task.LifetimeTerminationCompleted))
	}
	closed = append(closed, newPlacedTaskInPool(t, store, custodyTestTaskID("a"), newCustodyTestPlacement(t), task.LifetimeTerminationBudget))
	open := newPlacedTaskInPool(t, store, custodyTestTaskID("6"), shared, custodyLifetimeOpen)
	missing := []*task.LifetimeRegistrationRecord{
		newPlacedTaskInPool(t, store, custodyTestTaskID("3"), newCustodyTestPlacement(t), task.LifetimeTerminationCompleted),
		newPlacedTaskInPool(t, store, custodyTestTaskID("8"), newCustodyTestPlacement(t), task.LifetimeTerminationBudget),
	}
	if err := os.Mkdir(filepath.Join(store.Root, "tasks", custodyTestTaskID("1")), 0o700); err != nil {
		t.Fatal(err)
	}
	var restores []func()
	for _, registration := range missing {
		restores = append(restores, moveCustodyTestPool(t, registration.CustodyPlacement))
	}

	descriptors := openDescriptorCount(t)
	err := releasePrivateTerminalCustody(t.Context(), store.Root, task.SupervisorRef{}, pueue.PrivateTerminalCustodyScope{Full: true})
	if after := openDescriptorCount(t); after != descriptors {
		t.Fatalf("the pass left %d descriptors open, want %d", after, descriptors)
	}
	for _, restore := range restores {
		restore()
	}
	first, second := strings.Index(fmt.Sprint(err), missing[0].TaskID), strings.Index(fmt.Sprint(err), missing[1].TaskID)
	if first < 0 || second < first {
		t.Fatalf("full custody fault = %v, want both missing pools' tasks in inventory order", err)
	}
	for _, registration := range closed {
		if custodyTestPending(t, registration) {
			t.Fatalf("closed task %s kept custody beside an independent pool fault", registration.TaskID)
		}
	}
	for _, registration := range append([]*task.LifetimeRegistrationRecord{open}, missing...) {
		if !custodyTestPending(t, registration) {
			t.Fatalf("task %s released custody from an open lifetime or a missing pool", registration.TaskID)
		}
	}

	runTaskCustodyOwner(t, store.Root)
	if custodyTestPending(t, missing[0]) || custodyTestPending(t, missing[1]) || !custodyTestPending(t, open) {
		t.Fatal("the next full pass did not release only the restored closed pools")
	}
}

// heldStoreObservers holds one observer handle per task on the shared store
// until every concurrent release has opened its own, as a public status may
// hold a task while maintenance releases it. Arrivals past the barrier pass
// straight through.
type heldStoreObservers struct {
	mu      sync.Mutex
	arrived int
	full    chan struct{}
}

func (h *heldStoreObservers) arrive() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.arrived++
	if h.arrived == closedTaskCustodyWorkers {
		close(h.full)
	}
	return h.full
}

// TestConcurrentTaskCustodyReleasesShareOneStore runs the production
// coordinator with the canonical per-task release over one shared store. Each
// release first opens an observer handle of its task and waits until
// closedTaskCustodyWorkers observers are open at once; only then does the
// canonical release open its own handle of the same task, release the exact
// recorded pool and close that handle, while the observer handle stays open.
// Every closed task is released, the open lifetime is kept, and once the
// observers close every descriptor is returned. The watchdog only escapes a
// hang: firing is a failure, and the coordinator is still joined.
func TestConcurrentTaskCustodyReleasesShareOneStore(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	// Registered before any task, so the store closes after their handles.
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	shared := newCustodyTestPlacement(t)
	var ids []string
	var closed []*task.LifetimeRegistrationRecord
	for _, digit := range []string{"2", "3", "4", "5", "7", "8", "9"} {
		closed = append(closed, newPlacedTaskInPool(t, store, custodyTestTaskID(digit), shared, task.LifetimeTerminationCompleted))
		ids = append(ids, custodyTestTaskID(digit))
	}
	open := newPlacedTaskInPool(t, store, custodyTestTaskID("6"), shared, custodyLifetimeOpen)
	ids = append(ids, open.TaskID)

	descriptors := openDescriptorCount(t)
	barrier := &heldStoreObservers{full: make(chan struct{})}
	var observerFaults []error
	var observerMu sync.Mutex
	release := func(ctx context.Context, id string) error {
		observer, openErr := store.OpenTask(id)
		if openErr != nil {
			return openErr
		}
		defer func() {
			observerMu.Lock()
			defer observerMu.Unlock()
			if closeErr := observer.Close(); closeErr != nil {
				observerFaults = append(observerFaults, closeErr)
			}
		}()
		select {
		case <-barrier.arrive():
		case <-time.After(composedWatchdog):
			return fmt.Errorf("%w: fewer than %d releases overlapped", errComposedWatchdog, closedTaskCustodyWorkers)
		}
		return releaseStoredTaskCustody(ctx, store, id)
	}
	if err = releaseTaskCustodyInOrder(t.Context(), ids, release); err != nil {
		t.Fatal(err)
	}
	if len(observerFaults) != 0 {
		t.Fatalf("observer handles failed to close: %v", observerFaults)
	}
	if after := openDescriptorCount(t); after != descriptors {
		t.Fatalf("concurrent releases left %d descriptors open, want %d", after, descriptors)
	}
	for _, registration := range closed {
		if custodyTestPending(t, registration) {
			t.Fatalf("closed task %s kept custody", registration.TaskID)
		}
	}
	if !custodyTestPending(t, open) {
		t.Fatal("an open lifetime sharing the pool was released")
	}
}
