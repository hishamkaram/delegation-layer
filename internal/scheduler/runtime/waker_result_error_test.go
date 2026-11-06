package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/pulse"
)

// This fixture owns an actual open descriptor. It tests error-graph transport,
// not the production decision that a task's cleanup is unresolved.
type retainedWakeFileError struct {
	file  *os.File
	cause error
}

func (e *retainedWakeFileError) Error() string { return "retained wake test descriptor" }
func (e *retainedWakeFileError) Unwrap() error { return e.cause }

func newRetainedWakeFileError(t *testing.T) *retainedWakeFileError {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "retained-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	return &retainedWakeFileError{file: file, cause: errors.New("descriptor cleanup fault")}
}

func newJoiningWakeFault(t *testing.T) (joiningWaker, func()) {
	t.Helper()
	source, err := pulse.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return joiningWaker{
		inner: source, entered: make(chan context.Context, 1), returned: make(chan struct{}), release: gate,
		fault: errors.New("wake source fault while joining selected result"),
	}, release
}

func TestSchedulerSelectedResultRetainsCleanupGraphWhenJoiningWakerFails(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary task fault", true: "marked cleanup fault"}[marked], func(t *testing.T) {
			store := database(t)
			created := instant("2026-01-01T00:00:00Z")
			create(t, store, 1, created)
			prior := claimPrior(t, store, 1, 90, created, created.Add(time.Minute))
			clock := newClock(created.Add(90 * time.Second))
			waker, release := newJoiningWakeFault(t)
			retained := newRetainedWakeFileError(t)
			resultErr := error(retained)
			if marked {
				resultErr = fmt.Errorf("selected task: %w", errors.Join(ErrTaskCleanupUnresolved, retained))
			}
			l, parent := testLoop(t, Config{Store: store, Clock: clock, Waker: waker})
			ctx, cancel := context.WithCancel(parent)
			done, joined := make(chan error, 1), make(chan struct{})
			t.Cleanup(func() { cancel(); release(); receive(t, joined) })
			go func() { defer close(joined); done <- l.wait(ctx) }()
			receive(t, waker.entered)
			l.results <- taskResult{occurrence: prior, state: absent(clock.Now()), err: resultErr}
			receive(t, waker.returned)
			release()
			err := receive(t, done)
			if !errors.Is(err, waker.fault) {
				t.Fatalf("joining fault lost: %v", err)
			}
			if marked {
				assertRetainedWakeGraph(t, err, retained)
			} else if errors.Is(err, retained) {
				t.Fatalf("ordinary task fault became fatal: %v", err)
			}
			assertWakeOccurrenceUnchanged(t, parent, store, prior)
		})
	}
}

func TestSchedulerRunJoinsRemainingWorkerAfterSelectedResultAndWakerFault(t *testing.T) {
	store := database(t)
	created := instant("2026-01-01T00:00:00Z")
	create(t, store, 1, created)
	create(t, store, 2, created)
	clock := newClock(created.Add(30 * time.Second))
	initial := newWaker()
	waker, releaseWake := newJoiningWakeFault(t)
	retained := newRetainedWakeFileError(t)
	firstGate, secondGate := make(chan struct{}), make(chan struct{})
	var firstOnce, secondOnce sync.Once
	releaseFirst := func() { firstOnce.Do(func() { close(firstGate) }) }
	releaseSecond := func() { secondOnce.Do(func() { close(secondGate) }) }
	started := make(chan DispatchRequest, 2)
	secondFault := errors.New("remaining worker cleanup fault")
	tasks := taskFuncs{dispatch: func(request DispatchRequest) (TaskState, error) {
		started <- request
		if request.Definition.ScheduleID == id(1) {
			<-firstGate
			return absent(clock.Now()), errors.Join(ErrTaskCleanupUnresolved, retained)
		}
		<-secondGate
		return absent(clock.Now()), errors.Join(ErrTaskCleanupUnresolved, secondFault)
	}}
	firstWait := true
	wake := wakerFunc(func(ctx context.Context) error {
		if firstWait {
			firstWait = false
			return initial.Wait(ctx)
		}
		return waker.Wait(ctx)
	})
	ctx, cancel := context.WithCancel(t.Context())
	done, joined := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() { cancel(); releaseFirst(); releaseSecond(); releaseWake(); receive(t, joined) })
	go func() {
		defer close(joined)
		done <- Run(ctx, Config{Store: store, Tasks: tasks, Clock: clock, Waker: wake})
	}()
	wait := nextWait(t, initial)
	clock.set(created.Add(time.Minute))
	close(wait.wake)
	requests := []DispatchRequest{receive(t, started), receive(t, started)}
	before := make([]contract.Occurrence, 0, len(requests))
	for _, request := range requests {
		occurrence, err := store.GetOccurrence(t.Context(), request.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, occurrence)
	}
	receive(t, waker.entered)
	releaseFirst()
	receive(t, waker.returned)
	releaseWake()
	select {
	case err := <-done:
		t.Fatalf("Run returned before remaining worker settled: %v", err)
	default:
	}
	releaseSecond()
	err := receive(t, done)
	assertRetainedWakeGraph(t, err, retained)
	if !errors.Is(err, waker.fault) || !errors.Is(err, secondFault) {
		t.Fatalf("Run lost joined waker/remaining-worker causes: %v", err)
	}
	for _, occurrence := range before {
		assertWakeOccurrenceUnchanged(t, t.Context(), store, occurrence)
	}
}

func assertRetainedWakeGraph(t *testing.T, err error, retained *retainedWakeFileError) {
	t.Helper()
	var owner *retainedWakeFileError
	if !errors.Is(err, ErrTaskCleanupUnresolved) || !errors.Is(err, retained.cause) || !errors.As(err, &owner) || owner != retained {
		t.Fatalf("selected cleanup graph lost its original resource owner: %v", err)
	}
	if _, err := owner.file.Stat(); err != nil {
		t.Fatalf("retained descriptor closed before its cleanup owner: %v", err)
	}
}

func assertWakeOccurrenceUnchanged(t *testing.T, ctx context.Context, store contract.Store, before contract.Occurrence) {
	t.Helper()
	after, err := store.GetOccurrence(ctx, before.TaskID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("joining waker fault changed occurrence: before=%+v after=%+v err=%v", before, after, err)
	}
}
