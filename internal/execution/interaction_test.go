package execution

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type interactionFixtureProtocol struct{ done bool }

func (p *interactionFixtureProtocol) Start([]byte) ([][]byte, error) { return nil, nil }
func (p *interactionFixtureProtocol) Receive(line []byte) (InteractionUpdate, error) {
	switch string(line) {
	case "ready":
		return InteractionUpdate{Events: []task.InteractionEvent{{Kind: "ready", TurnID: "turn-1", Capabilities: &task.InteractionCapabilities{Steering: true}}}}, nil
	case "done":
		p.done = true
		return InteractionUpdate{Done: true}, nil
	default:
		return InteractionUpdate{}, task.ErrEvidenceFault
	}
}

func (p *interactionFixtureProtocol) Encode(message task.InteractionMessage) ([]byte, error) {
	return []byte(message.Text), nil
}

func (p *interactionFixtureProtocol) Complete() error {
	if !p.done {
		return task.ErrEvidenceFault
	}
	return nil
}

func preparedInteraction(t *testing.T) (*invocation, *interactiveInvocation) {
	t.Helper()
	td, permit, _, _ := fixtureTaskWithPlan(t, "echo", func(meta *task.MetaRecord, _ *Plan) { meta.RequestedConfig.Interactive = true })
	require(t, permit.Consume())
	t.Cleanup(func() { require(t, permit.Release()) })
	require(t, td.RecordStarted(0))
	invocation, err := prepareInvocation(td)
	require(t, err)
	t.Cleanup(func() { require(t, invocation.discard()) })
	_, err = invocation.prepareInteraction(&interactionFixtureProtocol{})
	require(t, err)
	return invocation, invocation.interaction
}

func TestInteractionDeliveryAndNaturalInputClosure(t *testing.T) {
	invocation, c := preparedInteraction(t)
	require(t, c.consume([]byte("ready\n")))
	message := task.InteractionMessage{ID: strings.Repeat("a", 32), Kind: "steer", Text: "literal $HOME guidance"}
	_, err := c.td.SubmitInteraction(message)
	require(t, err)
	budget, _ := armBudget(realClock{}, time.Minute, Options{})
	clock := newManualClock()
	c.start(budget, clock, nil)
	clock.timer.ch <- clock.now
	line, err := bufio.NewReader(invocation.stdin).ReadString('\n')
	require(t, err)
	if line != message.Text+"\n" {
		t.Fatalf("native message changed: %q", line)
	}
	c.Observe([]byte("done\n"))
	require(t, c.Complete())
	budget.complete(Options{})
	require(t, budget.await())
	extra, err := io.ReadAll(invocation.stdin)
	require(t, err)
	if len(extra) != 0 {
		t.Fatalf("unexpected extra native input: %q", extra)
	}
	next, err := c.td.NextInteraction()
	require(t, err)
	if next != nil {
		t.Fatal("delivered message became eligible for replay")
	}
	require(t, c.td.SealInteraction())
}

func TestInteractionProtocolFailureClosesInputAndDrainsFurtherCapture(t *testing.T) {
	invocation, c := preparedInteraction(t)
	budget, _ := armBudget(realClock{}, time.Minute, Options{})
	c.start(budget, realClock{}, nil)
	c.Observe([]byte("malformed\n"))
	<-c.protocolDone
	// Observe after failure must not retain data or strand the stdout capturer.
	c.Observe([]byte(strings.Repeat("x", task.MaxControlRecordSize)))
	if err := c.Complete(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("protocol fault hidden: %v", err)
	}
	budget.complete(Options{})
	require(t, budget.await())
	data, err := io.ReadAll(invocation.stdin)
	require(t, err)
	if len(data) != 0 {
		t.Fatal("protocol failure sent unexpected bytes")
	}
}

func TestInteractionBudgetClosesInputWithoutAnotherLaunch(t *testing.T) {
	invocation, c := preparedInteraction(t)
	clock := newManualClock()
	budget, _ := armBudget(clock, time.Minute, Options{})
	c.start(budget, realClock{}, nil)
	clock.timer.ch <- clock.now.Add(time.Minute)
	<-budget.workContext.Done()
	if err := c.Complete(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unfinished turn became successful: %v", err)
	}
	budget.complete(Options{})
	if err := budget.await(); err == nil {
		t.Fatal("missing supervisor stop capability hidden")
	}
	data, err := io.ReadAll(invocation.stdin)
	require(t, err)
	if len(data) != 0 {
		t.Fatal("budget expiry wrote native input")
	}
}

func TestInteractionCompletionPreservesUncertainConcurrentDelivery(t *testing.T) {
	_, c := preparedInteraction(t)
	require(t, c.consume([]byte("ready\n")))
	id := strings.Repeat("b", 32)
	_, err := c.td.SubmitInteraction(task.InteractionMessage{ID: id, Kind: "steer", Text: "late guidance"})
	require(t, err)
	budget, _ := armBudget(realClock{}, time.Minute, Options{})
	c.budget = budget
	// Hold the writer until after native completion closes input. This ordering
	// also covers a write already blocked when completion arrives.
	require(t, c.deliver())
	require(t, c.consume([]byte("done\n")))
	c.start(budget, realClock{}, nil)
	require(t, c.Complete())
	budget.complete(Options{})
	require(t, budget.await())
	state, err := c.td.InteractionState()
	require(t, err)
	if !state.Closed || len(state.Deliveries) != 1 || state.Deliveries[0].State != "unknown" {
		t.Fatalf("concurrent delivery gained false certainty: %+v", state)
	}
	message, err := c.td.NextInteraction()
	require(t, err)
	if message != nil {
		t.Fatal("uncertain delivery became eligible for replay")
	}
	// An unrelated writer fault must remain fatal, even after native success.
	c.writeFault = io.ErrShortWrite
	if !errors.Is(c.completionError(), io.ErrShortWrite) {
		t.Fatal("unrelated writer failure hidden")
	}
}
