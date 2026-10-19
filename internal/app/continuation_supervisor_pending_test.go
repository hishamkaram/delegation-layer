package app

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// supervisorStatusGate withholds one completed `status --json` result at its
// Pending.Done publication. The supervisor command stays unfinished until the
// test releases it; cleanup always releases and joins the held observer.
type supervisorStatusGate struct {
	hold     int
	held     chan struct{}
	release  chan struct{}
	returned chan struct{}

	mu       sync.Mutex
	seen     int
	released sync.Once
}

func newSupervisorStatusGate(t *testing.T, hold int) *supervisorStatusGate {
	t.Helper()
	gate := &supervisorStatusGate{hold: hold, held: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
	t.Cleanup(func() {
		gate.open()
		select {
		case <-gate.held:
		default:
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		waitInspectionWorkerTimeoutSignal(t, ctx, gate.returned, "held supervisor status")
	})
	return gate
}

func (g *supervisorStatusGate) observe(event pueue.CommandEvent) {
	if event.Stage != "completed" || !historicalCommandSequence(event.Argv, "status", "--json") {
		return
	}
	g.mu.Lock()
	g.seen++
	current := g.seen
	g.mu.Unlock()
	if current != g.hold {
		return
	}
	defer close(g.returned)
	close(g.held)
	<-g.release
}

func (g *supervisorStatusGate) open() { g.released.Do(func() { close(g.release) }) }

// requireStillHeld proves the gated supervisor command was in flight when the
// public command returned.
func (g *supervisorStatusGate) requireStillHeld(t *testing.T) {
	t.Helper()
	select {
	case <-g.held:
	default:
		t.Fatal("the supervisor task observation never reached the gate")
	}
	select {
	case <-g.returned:
		t.Fatal("the gated supervisor command completed before the public command returned")
	default:
	}
}

// requireInterruptedWithPendingSupervisor checks the allowed combination: a
// safely derived interruption offer beside an independent, unfinished
// supervisor-command observation. Pending publication is a different state.
func requireInterruptedWithPendingSupervisor(t *testing.T, fixture *rebootFixture, response Response) {
	t.Helper()
	if response.Status != "interrupted" || response.Pending == nil || response.Pending.Kind != string(tasklifecycle.PendingSupervisor) || response.Outcome != nil || response.Payload != nil {
		t.Fatalf("%s = status %q pending=%+v outcome=%+v, want interrupted with pending supervisor-command", response.Command, response.Status, response.Pending, response.Outcome)
	}
	requireInterruptedContinuation(t, fixture, response, "observed", true)
	failure := response.Failure
	if failure == nil || failure.Code != "task_interrupted" || failure.Stage != response.Command || failure.NextAction != "continue_task" {
		t.Fatalf("%s failure = %+v, want task_interrupted/continue_task", response.Command, failure)
	}
}

// A budget stop that still needs supervisor observation makes a watched
// collect reconcile the queue. The task observation is held past the watch
// boundary, so it stays an unfinished supervisor command while the closed
// lifetime independently proves the interruption.
func TestCollectInterruptionCoexistsWithPendingSupervisorCommand(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
	permit, err := fixture.td.PrepareStop("budget", "budget", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	// The client probe reads status first; the second read is the task
	// observation.
	gate := newSupervisorStatusGate(t, 2)
	fixture.options.Observer = gate.observe

	var stdout, stderr bytes.Buffer
	code := Run([]string{"collect", fixture.req.TaskID, "--root", fixture.root, "--watch", "2s", "--json"}, &stdout, &stderr, rebootDependencies(t, fixture))
	gate.requireStillHeld(t)
	response := decodeHistoricalResponse(t, stdout.Bytes(), stderr.Bytes(), code)
	if code != 1 {
		t.Fatalf("collect exit = %d %+v, want operational interruption 1", code, response)
	}
	requireInterruptedWithPendingSupervisor(t, fixture, response)
}

// Public status maps pure supervisor uncertainty to exit zero. Its private
// status observation is bounded by a fixed 30-second window, so the test runs
// the real status command in a synctest bubble. The gate holds the task
// observation unfinished; once the window expires in fake time, status
// reports the in-flight command beside the durable interruption offer.
func TestStatusInterruptionCoexistsWithPendingSupervisorCommand(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
	synctest.Test(t, func(t *testing.T) {
		// The binding probe reads status first; the second read is the task
		// observation.
		gate := newSupervisorStatusGate(t, 2)
		fixture.options.Observer = gate.observe

		code, response := runRebootStatusJSON(t, fixture)
		gate.requireStillHeld(t)
		if code != 0 || !strings.Contains(response.Error, pueue.ErrInFlight.Error()) {
			t.Fatalf("status = code %d error=%q, want exit 0 retaining %q", code, response.Error, pueue.ErrInFlight)
		}
		requireInterruptedWithPendingSupervisor(t, fixture, response)
	})
}
