package execution

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	protocolFloodLine  = "flood-after-fault\n"
	protocolFloodLines = 60000
	protocolDiagnostic = "capture_error: native protocol did not complete: "
)

var (
	errFixtureIncomplete = errors.New("fixture protocol did not observe done")
	errFixtureMalformed  = errors.New("fixture protocol rejected a record")
	errFixtureBrief      = errors.New("fixture protocol received another brief")
)

// protocolHelperScript is the child side of a deterministic line protocol:
// "<x" reads frame x, ">x" writes record x, and "<eof" requires that no other
// input arrives before natural EOF.
func protocolHelperScript(mode string) ([]string, bool) {
	script, ok := map[string][]string{
		"protocol-exchange":     {"<start", ">request", "<response", ">done", "<eof", ">late"},
		"protocol-incomplete":   {"<start", ">request", "<response"},
		"protocol-wait":         {"<start", ">waiting", "<eof"},
		"protocol-event":        {"<start", ">event", "<eof"},
		"protocol-done-event":   {"<start", ">done", "<eof", ">event"},
		"protocol-done-request": {"<start", ">done", "<eof", ">request"},
		"protocol-flood":        {"<start", ">malformed", "<eof", ">flood"},
		"protocol-interactive":  {">ready", ">done", "<eof"},
	}[mode]
	return script, ok
}

func runProtocolHelper(script []string) error {
	input := bufio.NewReader(os.Stdin)
	for _, step := range script {
		if err := protocolHelperStep(input, step); err != nil {
			_, writeErr := fmt.Fprintln(os.Stderr, err)
			return errors.Join(err, writeErr)
		}
	}
	return nil
}

func protocolHelperStep(input *bufio.Reader, step string) error {
	switch {
	case step == "<eof":
		rest, err := io.ReadAll(input)
		if err == nil && len(rest) != 0 {
			err = fmt.Errorf("input after protocol end: %q", rest)
		}
		if err != nil {
			return err
		}
		_, err = io.WriteString(os.Stderr, "stdin-eof\n")
		return err
	case step == ">flood":
		_, err := io.WriteString(os.Stdout, strings.Repeat(protocolFloodLine, protocolFloodLines))
		return err
	case strings.HasPrefix(step, "<"):
		line, err := input.ReadString('\n')
		if err == nil && line != step[1:]+"\n" {
			err = fmt.Errorf("unexpected frame %q", line)
		}
		return err
	default:
		_, err := io.WriteString(os.Stdout, step[1:]+"\n")
		return err
	}
}

// machineProtocolFixture has no Encode method: it is a machine Protocol only.
// It opts into record capture only when useRecord is set.
type machineProtocolFixture struct {
	brief       []byte
	onLine      func(string)
	useRecord   bool
	lines       []string
	recordCalls int
	done        bool
}

var _ CurrentRecordInteractionProtocol = (*machineProtocolFixture)(nil)

func (p *machineProtocolFixture) Start(brief []byte) ([][]byte, error) {
	if !bytes.Equal(brief, p.brief) {
		return nil, errFixtureBrief
	}
	return [][]byte{[]byte("start")}, nil
}

func (p *machineProtocolFixture) Receive(line []byte) (InteractionUpdate, error) {
	p.lines = append(p.lines, string(line))
	if p.onLine != nil {
		p.onLine(string(line))
	}
	switch string(line) {
	case "request":
		return InteractionUpdate{Outbound: [][]byte{[]byte("response")}}, nil
	case "done":
		p.done = true
		return InteractionUpdate{Done: true}, nil
	case "waiting", "late":
		return InteractionUpdate{}, nil
	case "event":
		return InteractionUpdate{Events: []task.InteractionEvent{{Kind: "ready"}}, Outbound: [][]byte{[]byte("leak")}}, nil
	default:
		return InteractionUpdate{}, errFixtureMalformed
	}
}

func (p *machineProtocolFixture) ReceiveRecord(v providerjson.Value) (InteractionUpdate, error) {
	p.recordCalls++
	data, err := v.Raw(task.MaxControlRecordSize)
	if err != nil {
		return InteractionUpdate{}, err
	}
	return p.Receive(data)
}

func (p *machineProtocolFixture) UseRecordCapture() bool { return p.useRecord }

func (p *machineProtocolFixture) Complete() error {
	if !p.done {
		return errFixtureIncomplete
	}
	return nil
}

type discardIdentityObserver struct{}

func (discardIdentityObserver) Observe([]byte)  {}
func (discardIdentityObserver) Complete() error { return nil }

type countingIdentityObserver struct{ observed, completed int }

func (o *countingIdentityObserver) Observe([]byte) { o.observed++ }
func (o *countingIdentityObserver) Complete() error {
	o.completed++
	return nil
}

// echoedMachine reports each received line and is complete once the test has
// applied Done directly, so no process timing decides the outcome.
type echoedMachine struct{ lines chan string }

func (p *echoedMachine) Start([]byte) ([][]byte, error) { return nil, nil }
func (p *echoedMachine) Receive(line []byte) (InteractionUpdate, error) {
	p.lines <- string(line)
	return InteractionUpdate{}, nil
}
func (p *echoedMachine) Complete() error { return nil }

func runMachineProtocol(t *testing.T, mode string, protocol *machineProtocolFixture, opts Options) (*taskdir.TaskDir, *eventLog, Result) {
	t.Helper()
	td, permit, plan, brief := fixtureTask(t, mode)
	protocol.brief = brief
	events := &eventLog{}
	opts.Protocol = protocol
	opts.Hooks.Event = events.add
	opts.Hooks.Start = func(cmd *exec.Cmd) error {
		events.add("start-hook")
		return cmd.Start()
	}
	return td, events, Run(td, permit, plan, opts)
}

func assertSingleStart(t *testing.T, events *eventLog) {
	t.Helper()
	snapshot := events.snapshot()
	hooks, started := 0, 0
	for _, name := range snapshot {
		if name == "start-hook" {
			hooks++
		}
		if name == "started" {
			started++
		}
	}
	if hooks != 1 || started != 1 {
		t.Fatalf("provider start was not exactly once: %v", snapshot)
	}
}

func assertNoInteractionJournal(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	for _, name := range []string{"interaction", ".interaction.lock"} {
		if _, err := os.Stat(filepath.Join(td.Dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("machine protocol touched the interaction journal %s: %v", name, err)
		}
	}
}

func rawStream(t *testing.T, td *taskdir.TaskDir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(td.Dir, "raw", name))
	require(t, err)
	return data
}

func assertProtocolRejected(t *testing.T, td *taskdir.TaskDir, r Result, cause error) {
	t.Helper()
	require(t, errors.Join(r.Error, r.CleanupError, r.StopError))
	if !errors.Is(r.ReceiptError, cause) {
		t.Fatalf("protocol fault was not reported: %v", r.ReceiptError)
	}
	if r.Outcome == nil || r.Outcome.Verdict != task.VerdictRejected {
		t.Fatalf("protocol fault was not sealed as a rejection: %+v", r.Outcome)
	}
	if got := string(payload(t, td, r.Outcome)); !strings.HasPrefix(got, protocolDiagnostic) {
		t.Fatalf("seal lost the protocol diagnostic: %q", got)
	}
	assertNoInteractionJournal(t, td)
}

func TestValidateProtocolSelectionCoversEveryCombination(t *testing.T) {
	for _, testCase := range []struct {
		interactive, protocol, interaction, valid bool
	}{
		{interactive: false, protocol: false, interaction: false, valid: true},
		{interactive: false, protocol: true, interaction: false, valid: true},
		{interactive: true, protocol: false, interaction: true, valid: true},
		{interactive: false, protocol: false, interaction: true},
		{interactive: false, protocol: true, interaction: true},
		{interactive: true, protocol: false, interaction: false},
		{interactive: true, protocol: true, interaction: false},
		{interactive: true, protocol: true, interaction: true},
	} {
		err := ValidateProtocolSelection(testCase.interactive, testCase.protocol, testCase.interaction)
		if testCase.valid && err != nil {
			t.Fatalf("valid selection %+v refused: %v", testCase, err)
		}
		if !testCase.valid && !errors.Is(err, task.ErrInteractionUnavailable) {
			t.Fatalf("invalid selection %+v accepted: %v", testCase, err)
		}
	}
}

func TestRunRejectsInvalidProtocolSelectionBeforeStart(t *testing.T) {
	for _, testCase := range []struct {
		name                            string
		interactive, protocol, interact bool
	}{
		{name: "interactive-without-protocol", interactive: true},
		{name: "interactive-with-machine-protocol", interactive: true, protocol: true},
		{name: "interactive-with-both", interactive: true, protocol: true, interact: true},
		{name: "ordinary-with-both", protocol: true, interact: true},
		{name: "ordinary-with-interaction", interact: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			td, permit, plan, _ := fixtureTaskWithPlan(t, "protocol-exchange", func(meta *task.MetaRecord, _ *Plan) {
				meta.RequestedConfig.Interactive = testCase.interactive
			})
			events := &eventLog{}
			opts := Options{Hooks: Hooks{Event: events.add}}
			if testCase.protocol {
				opts.Protocol = &machineProtocolFixture{}
			}
			if testCase.interact {
				opts.Interaction = &interactionFixtureProtocol{}
			}
			r := Run(td, permit, plan, opts)
			if !errors.Is(r.Error, task.ErrInteractionUnavailable) || r.Outcome != nil {
				t.Fatalf("invalid selection was not refused: %+v", r)
			}
			assertRefusedBeforeStart(t, td, events)
		})
	}
}

func TestRunRejectsIdentityObserverWithProtocol(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "protocol-exchange")
	events := &eventLog{}
	r := Run(td, permit, plan, Options{Protocol: &machineProtocolFixture{}, Identity: discardIdentityObserver{}, Hooks: Hooks{Event: events.add}})
	if !errors.Is(r.Error, errProtocolIdentityObserver) || r.Outcome != nil {
		t.Fatalf("dropped identity observer was not refused: %+v", r)
	}
	assertRefusedBeforeStart(t, td, events)
}

func assertRefusedBeforeStart(t *testing.T, td *taskdir.TaskDir, events *eventLog) {
	t.Helper()
	if slices.Contains(events.snapshot(), "start-entry") {
		t.Fatalf("refused selection reached launch preparation: %v", events.snapshot())
	}
	inspection, err := td.Inspect()
	require(t, err)
	if inspection.StartedExists || inspection.SealExists || inspection.OutcomeExists {
		t.Fatalf("refused selection left launch evidence: %+v", inspection)
	}
	assertNoInteractionJournal(t, td)
}

func TestRunInteractiveSelectionSupersedesSuppliedIdentityObserver(t *testing.T) {
	td, permit, plan, _ := fixtureTaskWithPlan(t, "protocol-interactive", func(meta *task.MetaRecord, _ *Plan) {
		meta.RequestedConfig.Interactive = true
	})
	observer := &countingIdentityObserver{}
	assertSuccess(t, Run(td, permit, plan, Options{Interaction: &interactionFixtureProtocol{}, Identity: observer}))
	if observer.observed != 0 || observer.completed != 0 {
		t.Fatalf("historical interaction invoked the superseded observer: %+v", observer)
	}
	state, err := td.InteractionState()
	require(t, err)
	if !state.Closed {
		t.Fatalf("interactive journal was not closed and sealed: %+v", state)
	}
}

func TestRunInteractiveSelectionKeepsUserJournal(t *testing.T) {
	td, permit, plan, _ := fixtureTaskWithPlan(t, "protocol-interactive", func(meta *task.MetaRecord, _ *Plan) {
		meta.RequestedConfig.Interactive = true
	})
	assertSuccess(t, Run(td, permit, plan, Options{Interaction: &interactionFixtureProtocol{}}))
	state, err := td.InteractionState()
	require(t, err)
	if !state.Closed {
		t.Fatalf("interactive journal was not closed and sealed: %+v", state)
	}
}

func TestMachineProtocolExchangeSealsWithoutInteractionJournal(t *testing.T) {
	for _, useRecord := range []bool{false, true} {
		t.Run(fmt.Sprintf("record-capture=%t", useRecord), func(t *testing.T) {
			protocol := &machineProtocolFixture{useRecord: useRecord}
			td, events, r := runMachineProtocol(t, "protocol-exchange", protocol, Options{})
			assertSuccess(t, r)
			if got := string(payload(t, td, r.Outcome)); got != "request\ndone\nlate\n" {
				t.Fatalf("raw protocol output changed: %q", got)
			}
			// The record after Done is still parsed and validated.
			if !slices.Equal(protocol.lines, []string{"request", "done", "late"}) {
				t.Fatalf("protocol records were not all received: %q", protocol.lines)
			}
			wantRecordCalls := 0
			if useRecord {
				wantRecordCalls = len(protocol.lines)
			}
			if protocol.recordCalls != wantRecordCalls {
				t.Fatalf("record capture opt-in changed: calls=%d", protocol.recordCalls)
			}
			if got := string(rawStream(t, td, "stderr")); got != "stdin-eof\n" {
				t.Fatalf("Done did not end native input after the response: %q", got)
			}
			assertSingleStart(t, events)
			assertNoInteractionJournal(t, td)
		})
	}
}

func TestMachineProtocolIncompleteEOFSealsCompletionDiagnostic(t *testing.T) {
	td, events, r := runMachineProtocol(t, "protocol-incomplete", &machineProtocolFixture{}, Options{})
	assertProtocolRejected(t, td, r, errFixtureIncomplete)
	if got := string(rawStream(t, td, "stdout")); got != "request\n" {
		t.Fatalf("raw evidence changed: %q", got)
	}
	assertSingleStart(t, events)
}

func TestMachineProtocolDeadlineClosesInputOnceWithoutRelaunch(t *testing.T) {
	clock := newManualClock()
	stops := &eventLog{}
	protocol := &machineProtocolFixture{onLine: func(line string) {
		if line == "waiting" {
			clock.timer.ch <- clock.now.Add(time.Minute)
		}
	}}
	opts := Options{Clock: clock, Stopper: stopperFunc(func(context.Context, time.Time) error {
		stops.add("stop")
		return nil
	})}
	td, events, r := runMachineProtocol(t, "protocol-wait", protocol, opts)
	assertProtocolRejected(t, td, r, errFixtureIncomplete)
	if errors.Is(r.ReceiptError, os.ErrClosed) {
		t.Fatalf("native input was closed more than once: %v", r.ReceiptError)
	}
	if got := string(rawStream(t, td, "stderr")); got != "stdin-eof\n" {
		t.Fatalf("deadline did not close native input: %q", got)
	}
	eventIndex(t, events.snapshot(), "deadline-observed")
	assertSingleStart(t, events)
	if len(stops.snapshot()) != 1 {
		t.Fatalf("budget stop was not requested exactly once: %v", stops.snapshot())
	}
}

func TestMachineProtocolFaultDrainsRawCapture(t *testing.T) {
	td, events, r := runMachineProtocol(t, "protocol-flood", &machineProtocolFixture{}, Options{})
	assertProtocolRejected(t, td, r, errFixtureMalformed)
	stdout := rawStream(t, td, "stdout")
	if len(stdout) != len("malformed\n")+len(protocolFloodLine)*protocolFloodLines {
		t.Fatalf("raw capture stopped after the protocol fault: %d bytes", len(stdout))
	}
	if got := string(rawStream(t, td, "stderr")); got != "stdin-eof\n" {
		t.Fatalf("protocol fault wrote native input: %q", got)
	}
	assertSingleStart(t, events)
}

func TestMachineProtocolEventsRejectBeforeOutbound(t *testing.T) {
	for _, testCase := range []struct {
		mode  string
		cause error
	}{
		{mode: "protocol-event", cause: errMachineProtocolEvents},
		{mode: "protocol-done-event", cause: errMachineProtocolEvents},
		{mode: "protocol-done-request", cause: task.ErrInteractionClosed},
	} {
		t.Run(testCase.mode, func(t *testing.T) {
			td, events, r := runMachineProtocol(t, testCase.mode, &machineProtocolFixture{}, Options{})
			assertProtocolRejected(t, td, r, testCase.cause)
			if got := string(rawStream(t, td, "stderr")); got != "stdin-eof\n" {
				t.Fatalf("rejected update reached native input: %q", got)
			}
			assertSingleStart(t, events)
		})
	}
}

// protocolHangWatchdog only bounds a wait that a regression would leave stuck.
// It is not a latency requirement and never orders events.
const protocolHangWatchdog = 5 * time.Second

// releaseFile closes f unless an earlier owner, such as the engine, already did.
func releaseFile(t *testing.T, f *os.File) error {
	t.Helper()
	if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

// testPipe returns a pipe whose ends are closed when the test ends, whatever
// its outcome. The engine may close the writer first.
func testPipe(t *testing.T) (reader, writer *os.File) {
	t.Helper()
	reader, writer, err := os.Pipe()
	require(t, err)
	t.Cleanup(func() {
		require(t, errors.Join(releaseFile(t, writer), releaseFile(t, reader)))
	})
	return reader, writer
}

// saturatedMachine queues a full outbound buffer, first frame included, and
// then applies Done before any writer exists. The returned bytes are exactly
// what a loss-free ordered close must deliver.
func saturatedMachine(t *testing.T, first []byte, input *os.File) (*interactiveInvocation, *echoedMachine, []byte) {
	t.Helper()
	protocol := &echoedMachine{lines: make(chan string, 1)}
	c := newInteractiveInvocation(nil, protocol, nil, input, nil)
	frames := [][]byte{first}
	for n := 1; n < outboundFrames; n++ {
		frames = append(frames, fmt.Appendf(nil, "frame-%d", n))
	}
	require(t, c.apply(InteractionUpdate{Outbound: frames}))
	if err := c.apply(InteractionUpdate{Outbound: [][]byte{[]byte("overflow")}}); err == nil {
		t.Fatal("queue saturation was not reported")
	}
	require(t, c.apply(InteractionUpdate{Done: true}))
	var want []byte
	for _, frame := range frames {
		want = append(append(want, frame...), '\n')
	}
	return c, protocol, want
}

// machineRun owns one started engine, its budget and the native-input reader.
// join is the only release order: closing the reader first frees a writer
// blocked on a stalled pipe, so the engine and then the budget can be joined.
type machineRun struct {
	c      *interactiveInvocation
	budget *budgetOwner
	reader *os.File
	once   sync.Once
	err    error
}

func newMachineRun(c *interactiveInvocation, reader *os.File) *machineRun {
	budget, _ := armBudget(realClock{}, time.Minute, Options{})
	c.start(budget, realClock{}, nil)
	return &machineRun{c: c, budget: budget, reader: reader}
}

func (m *machineRun) release(t *testing.T) {
	t.Helper()
	readerErr := releaseFile(t, m.reader)
	engineErr := m.c.Complete()
	m.budget.complete(Options{})
	m.err = errors.Join(readerErr, engineErr, m.budget.await())
}

// join releases the run once and returns the engine's completion result.
func (m *machineRun) join(t *testing.T) error {
	t.Helper()
	m.once.Do(func() { m.release(t) })
	return m.err
}

// startMachine registers the release at acquisition, so an assertion failure
// cannot leak the engine, budget or pipe. A release error is only a
// consequence of an earlier failure then, and is not reported again.
func startMachine(t *testing.T, c *interactiveInvocation, reader *os.File) *machineRun {
	t.Helper()
	m := newMachineRun(c, reader)
	t.Cleanup(func() {
		m.once.Do(func() {
			m.release(t)
			if m.err != nil && !t.Failed() {
				t.Errorf("machine release: %v", m.err)
			}
		})
	})
	return m
}

// drainMachine reads native stdin to EOF, which only the ordered close after
// the last queued frame can produce, and then joins the engine.
func drainMachine(t *testing.T, m *machineRun, want []byte) {
	t.Helper()
	require(t, m.reader.SetReadDeadline(time.Now().Add(protocolHangWatchdog)))
	got, err := io.ReadAll(m.reader)
	require(t, err)
	if !bytes.Equal(got, want) {
		t.Fatalf("Done discarded earlier frames: got %d bytes, want %d", len(got), len(want))
	}
	require(t, m.join(t))
}

func TestMachineDoneWithSaturatedQueueWritesEveryEarlierFrame(t *testing.T) {
	reader, writer := testPipe(t)
	c, _, want := saturatedMachine(t, []byte("frame-0"), writer)
	drainMachine(t, startMachine(t, c, reader), want)
}

func TestMachineDoneDoesNotBlockProtocolBehindBlockedWriter(t *testing.T) {
	reader, writer := testPipe(t)
	// No pipe buffer holds this frame, so the writer stays blocked until the
	// test reads; the protocol must keep consuming records meanwhile.
	c, protocol, want := saturatedMachine(t, bytes.Repeat([]byte("x"), 1<<20), writer)
	m := startMachine(t, c, reader)
	c.Observe([]byte("late\n"))
	select {
	case line := <-protocol.lines:
		if line != "late" {
			t.Fatalf("record after Done was not consumed: %q", line)
		}
	case <-time.After(protocolHangWatchdog):
		t.Fatal("protocol loop stalled behind the blocked writer")
	}
	drainMachine(t, m, want)
}

// A test that fails while the writer is blocked must still release it. This
// takes the cleanup path with no read, as an assertion failure would.
func TestMachineReleaseFreesBlockedWriterAndJoinsEngine(t *testing.T) {
	reader, writer := testPipe(t)
	c, _, _ := saturatedMachine(t, bytes.Repeat([]byte("x"), 1<<20), writer)
	m := newMachineRun(c, reader)
	joined := make(chan error, 1)
	go func() { joined <- m.join(t) }()
	select {
	case err := <-joined:
		if !errors.Is(err, brokenPipe(t)) {
			t.Fatalf("released writer did not fail on the closed reader: %v", err)
		}
	case <-time.After(protocolHangWatchdog):
		t.Fatal("release left the blocked writer or engine running")
	}
	select {
	case <-c.done:
	default:
		t.Fatal("engine was not joined by release")
	}
}

// brokenPipe returns the platform's write-to-closed-reader errno, obtained from
// the OS so the tests need no platform constant.
func brokenPipe(t *testing.T) error {
	t.Helper()
	reader, writer := testPipe(t)
	require(t, releaseFile(t, reader))
	_, writeErr := writer.Write([]byte{0})
	var pathErr *os.PathError
	if !errors.As(writeErr, &pathErr) {
		t.Fatalf("write to a pipe without a reader did not fail: %v", writeErr)
	}
	return pathErr.Err
}

func TestMachineWriteFailureAfterDoneIsReported(t *testing.T) {
	reader, writer := testPipe(t)
	require(t, releaseFile(t, reader))
	c := newInteractiveInvocation(nil, &echoedMachine{lines: make(chan string, 1)}, nil, writer, nil)
	require(t, c.apply(InteractionUpdate{Outbound: [][]byte{[]byte("reply")}}))
	require(t, c.apply(InteractionUpdate{Done: true}))
	if err := startMachine(t, c, reader).join(t); !errors.Is(err, brokenPipe(t)) {
		t.Fatalf("native completion forgave a lost machine reply: %v", err)
	}
}
