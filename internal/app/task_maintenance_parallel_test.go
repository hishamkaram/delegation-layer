package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

// gatedTaskCustody is a release stand-in whose every call blocks on its own
// task's gate. It records entries, completions and the peak number of calls
// active at once. Inside a synctest bubble, synctest.Wait makes each
// observation deterministic: every worker is then blocked on a gate or on the
// coordinator, so no further call can start without a test action. Test
// cleanup opens every gate, so a failed assertion still lets the coordinator
// and its workers finish before the bubble ends.
type gatedTaskCustody struct {
	mu        sync.Mutex
	gates     map[string]chan struct{}
	opens     map[string]func()
	results   map[string]error
	completes map[string]func()
	entered   []string
	completed []string
	active    int
	peak      int
}

func newGatedTaskCustody(t *testing.T, ids []string) *gatedTaskCustody {
	t.Helper()
	gated := &gatedTaskCustody{
		gates: make(map[string]chan struct{}, len(ids)), opens: make(map[string]func(), len(ids)),
		results: map[string]error{}, completes: map[string]func(){},
	}
	for _, id := range ids {
		gate := make(chan struct{})
		gated.gates[id], gated.opens[id] = gate, sync.OnceFunc(func() { close(gate) })
	}
	t.Cleanup(func() {
		for _, id := range ids {
			gated.open(id)
		}
	})
	return gated
}

func (g *gatedTaskCustody) release(_ context.Context, id string) error {
	g.mu.Lock()
	g.entered = append(g.entered, id)
	g.active++
	g.peak = max(g.peak, g.active)
	gate, complete := g.gates[id], g.completes[id]
	g.mu.Unlock()
	<-gate
	if complete != nil {
		complete()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	g.completed = append(g.completed, id)
	return g.results[id]
}

func (g *gatedTaskCustody) open(id string) { g.opens[id]() }

// lastHeld returns the held release latest in inventory order.
func (g *gatedTaskCustody) lastHeld(ids []string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	for index := len(ids) - 1; index >= 0; index-- {
		if slices.Contains(g.entered, ids[index]) && !slices.Contains(g.completed, ids[index]) {
			return ids[index]
		}
	}
	return ""
}

// sameTaskSet reports whether got holds exactly the ids in want. Workers that
// receive in inventory order may still enter their calls in any order.
func sameTaskSet(got, want []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want)))
}

// snapshot returns copies of the entries, completions, active and peak counts.
func (g *gatedTaskCustody) snapshot() (entered, completed []string, active, peak int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.entered), slices.Clone(g.completed), g.active, g.peak
}

// startTaskCustodyCoordinator runs the coordinator and returns its result
// channel, which receives exactly once when the coordinator returns.
func startTaskCustodyCoordinator(ctx context.Context, ids []string, release func(context.Context, string) error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- releaseTaskCustodyInOrder(ctx, ids, release) }()
	return done
}

func requireCoordinatorRunning(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("the coordinator returned %v while a started release was still active", err)
	default:
	}
}

func custodyTestIDs(count int) []string {
	ids := make([]string, count)
	for index := range ids {
		ids[index] = "task-" + strconv.Itoa(index)
	}
	return ids
}

// TestTaskCustodyCoordinatorBoundsOverlapAndCallsEachTaskOnce: with every
// release held, exactly min(closedTaskCustodyWorkers, len(ids)) run at once and
// no further call starts. The started calls are always an inventory prefix.
// Opening the gates one at a time in inventory order lets each freed worker
// take the next id, so every id is released exactly once and the coordinator
// returns only after the last.
func TestTaskCustodyCoordinatorBoundsOverlapAndCallsEachTaskOnce(t *testing.T) {
	for _, count := range []int{0, 1, 3, 6, 9} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requireBoundedTaskCustody(t, custodyTestIDs(count))
			})
		})
	}
}

func requireBoundedTaskCustody(t *testing.T, ids []string) {
	t.Helper()
	gated := newGatedTaskCustody(t, ids)
	done := startTaskCustodyCoordinator(t.Context(), ids, gated.release)
	for opened := range ids {
		synctest.Wait()
		entered, _, active, _ := gated.snapshot()
		if want := min(closedTaskCustodyWorkers, len(ids)-opened); active != want {
			t.Fatalf("after %d completions %d releases are active, want %d", opened, active, want)
		}
		if want := ids[:min(opened+closedTaskCustodyWorkers, len(ids))]; !sameTaskSet(entered, want) {
			t.Fatalf("after %d completions releases %v started, want %v", opened, entered, want)
		}
		requireCoordinatorRunning(t, done)
		gated.open(ids[opened])
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	entered, completed, _, peak := gated.snapshot()
	if !sameTaskSet(entered, ids) || !slices.Equal(completed, ids) {
		t.Fatalf("entered %v completed %v, want each of %v once", entered, completed, ids)
	}
	if want := min(closedTaskCustodyWorkers, len(ids)); peak != want {
		t.Fatalf("peak overlap = %d, want %d", peak, want)
	}
}

// custodyTestError is a concrete release fault a caller narrows with
// errors.As.
type custodyTestError struct{ id string }

func (f *custodyTestError) Error() string { return "custody fault " + f.id }

var (
	errCustodyTestPool  = errors.New("custody test pool unavailable")
	errCustodyTestClose = errors.New("custody test task close")
)

// TestTaskCustodyCoordinatorKeepsInventoryOrderOfFaults: later tasks finish
// first and several tasks fail, one with a release fault joined beside its
// handle close fault. Every task still runs, and the result keeps inventory
// order, each task id, each concrete fault for errors.As and every sentinel
// for errors.Is, with no cancellation marker.
func TestTaskCustodyCoordinatorKeepsInventoryOrderOfFaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ids := custodyTestIDs(6)
		gated := newGatedTaskCustody(t, ids)
		gated.results[ids[1]] = errCustodyTestPool
		gated.results[ids[3]] = errors.Join(&custodyTestError{id: ids[3]}, errCustodyTestClose)
		gated.results[ids[5]] = &custodyTestError{id: ids[5]}
		done := startTaskCustodyCoordinator(t.Context(), ids, gated.release)
		// Always open the held release latest in inventory order, so completion
		// runs 3, 4, 5, 2, 1, 0.
		for range ids {
			synctest.Wait()
			gated.open(gated.lastHeld(ids))
		}
		err := <-done
		_, completed, _, _ := gated.snapshot()
		if want := []string{ids[3], ids[4], ids[5], ids[2], ids[1], ids[0]}; !slices.Equal(completed, want) {
			t.Fatalf("completion order = %v, want %v", completed, want)
		}
		requireTaskCustodyFaults(t, err, ids[1], ids[3], ids[5])
		if !errors.Is(err, errCustodyTestPool) || !errors.Is(err, errCustodyTestClose) || errors.Is(err, context.Canceled) {
			t.Fatalf("faults = %v, want the pool and close faults and no cancellation", err)
		}
		var fault *custodyTestError
		if !errors.As(err, &fault) || fault.id != ids[3] {
			t.Fatalf("first concrete fault = %v, want task %s", fault, ids[3])
		}
	})
}

// requireTaskCustodyFaults requires err to join exactly one wrapped fault per
// faulted id, in inventory order, and returns the joined errors after them.
func requireTaskCustodyFaults(t *testing.T, err error, faulted ...string) []error {
	t.Helper()
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("faults = %v, want a joined result", err)
	}
	parts := joined.Unwrap()
	if len(parts) < len(faulted) {
		t.Fatalf("faults = %v, want one per task %v", parts, faulted)
	}
	for index, id := range faulted {
		if prefix := fmt.Sprintf("task %s custody release: ", id); !strings.HasPrefix(parts[index].Error(), prefix) {
			t.Fatalf("fault %d = %q, want task %s in inventory order", index, parts[index], id)
		}
	}
	return parts[len(faulted):]
}

// TestTaskCustodyCoordinatorRefusesAlreadyCanceledPass: a pass whose context
// is already canceled releases nothing and reports the cancellation once.
func TestTaskCustodyCoordinatorRefusesAlreadyCanceledPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		ids := custodyTestIDs(9)
		gated := newGatedTaskCustody(t, ids)
		err := releaseTaskCustodyInOrder(ctx, ids, gated.release)
		if entered, _, _, _ := gated.snapshot(); len(entered) != 0 {
			t.Fatalf("a canceled pass released %v", entered)
		}
		if trailing := requireTaskCustodyFaults(t, err); len(trailing) != 1 || !errors.Is(trailing[0], context.Canceled) {
			t.Fatalf("canceled pass = %v, want one cancellation", err)
		}
	})
}

// TestTaskCustodyCoordinatorJoinsBlockedReleasesAfterCancellation: the pass
// is canceled while four releases are held, either from outside while the
// coordinator is handing out the fifth id or by a held release as it returns,
// which frees its worker to receive the next id. The coordinator keeps
// waiting for the held releases, starts no later release once cancellation is
// observable, and returns their concrete faults in inventory order followed by
// one cancellation, even though one held release joined its own cancellation
// cause beside a concrete fault.
func TestTaskCustodyCoordinatorJoinsBlockedReleasesAfterCancellation(t *testing.T) {
	for _, byRelease := range []bool{false, true} {
		t.Run("by release "+strconv.FormatBool(byRelease), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requireCanceledPassJoinsHeldReleases(t, byRelease)
			})
		})
	}
}

func requireCanceledPassJoinsHeldReleases(t *testing.T, byRelease bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ids := custodyTestIDs(9)
	gated := newGatedTaskCustody(t, ids)
	gated.results[ids[0]] = errors.Join(&custodyTestError{id: ids[0]}, context.Canceled)
	gated.results[ids[2]] = errCustodyTestPool
	gated.completes[ids[3]] = func() {
		if byRelease {
			cancel()
		}
	}
	done := startTaskCustodyCoordinator(ctx, ids, gated.release)
	synctest.Wait()
	if byRelease {
		gated.open(ids[3])
	} else {
		cancel()
	}
	synctest.Wait()
	requireCoordinatorRunning(t, done)
	// Opening a gate is idempotent, so the release that canceled is reopened.
	for _, id := range []string{ids[3], ids[1], ids[0], ids[2]} {
		gated.open(id)
		synctest.Wait()
	}
	err := <-done
	entered, completed, active, _ := gated.snapshot()
	if want := ids[:closedTaskCustodyWorkers]; !sameTaskSet(entered, want) || len(completed) != len(want) || active != 0 {
		t.Fatalf("entered %v completed %v active %d, want only the held %v joined", entered, completed, active, want)
	}
	trailing := requireTaskCustodyFaults(t, err, ids[0], ids[2])
	if len(trailing) != 1 || !errors.Is(trailing[0], context.Canceled) {
		t.Fatalf("canceled pass = %v, want one trailing cancellation", err)
	}
	var fault *custodyTestError
	if !errors.As(err, &fault) || fault.id != ids[0] || !errors.Is(err, errCustodyTestPool) {
		t.Fatalf("canceled pass = %v, want the held releases' concrete faults", err)
	}
}
