package execution

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	interactionPollInterval = 250 * time.Millisecond
	// outboundFrames bounds frames queued for the sole stdin writer.
	outboundFrames = 16
)

type interactionFrame struct {
	data      []byte
	messageID string
}

// inactiveTimer never fires. A machine protocol has no user inbox to poll.
type inactiveTimer struct{}

func (inactiveTimer) C() <-chan time.Time { return nil }
func (inactiveTimer) Stop() bool          { return false }

// interactiveInvocation is the single protocol engine on the core transport.
// Its user channel is nil for a machine protocol, which never touches the
// interaction journal.
type interactiveInvocation struct {
	td           *taskdir.TaskDir
	protocol     Protocol
	user         InteractionProtocol
	input        *os.File
	brief        []byte
	chunks       chan []byte
	records      chan providerjson.Value
	outbound     chan interactionFrame
	written      chan string
	protocolDone chan struct{}
	writerDone   chan struct{}
	closerDone   chan struct{}
	done         chan struct{}
	budget       *budgetOwner
	clock        Clock
	pending      []byte
	finished     bool
	fault        error
	writeFault   error
	closeFault   error
	closeOnce    sync.Once
	// The runner lease owns mismatch latching; this callback only borrows it.
	validateProject func(context.Context) error
	// outboundOnce closes the frame queue: at machine Done, or when the engine ends.
	outboundOnce sync.Once
}

func (i *invocation) prepareInteraction(protocol Protocol, user InteractionProtocol) (IdentityObserver, error) {
	if protocol == nil {
		return nil, nil
	}
	brief, err := io.ReadAll(io.LimitReader(i.stdin, task.MaxBriefSize+1))
	if err != nil {
		return nil, err
	}
	if len(brief) > task.MaxBriefSize {
		return nil, task.ErrControlRecordTooBig
	}
	if user != nil {
		if err = i.td.BeginInteraction(); err != nil {
			return nil, err
		}
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	if err = i.stdin.Close(); err != nil {
		return nil, errors.Join(err, reader.Close(), writer.Close())
	}
	i.stdin = reader
	i.interaction = newInteractiveInvocation(i.td, protocol, user, writer, brief)
	return i.interaction, nil
}

func newInteractiveInvocation(td *taskdir.TaskDir, protocol Protocol, user InteractionProtocol, input *os.File, brief []byte) *interactiveInvocation {
	return &interactiveInvocation{
		td: td, protocol: protocol, user: user, input: input, brief: brief,
		records: make(chan providerjson.Value, 16), chunks: make(chan []byte, 16), outbound: make(chan interactionFrame, outboundFrames), written: make(chan string, 16),
		protocolDone: make(chan struct{}), writerDone: make(chan struct{}), closerDone: make(chan struct{}), done: make(chan struct{}),
	}
}

// Observe is called by the sole stdout capture worker, after raw bytes are saved.
// Backpressure is bounded; the independent stdin writer prevents a protocol
// response from blocking capture. A failed protocol continues draining raw output.
func (c *interactiveInvocation) Observe(data []byte) {
	copyData := bytes.Clone(data)
	select {
	case c.chunks <- copyData:
	case <-c.protocolDone:
	}
}

func (c *interactiveInvocation) ObserveRecord(v providerjson.Value) error {
	select {
	case c.records <- v:
	case <-c.protocolDone:
	}
	return nil
}

func (c *interactiveInvocation) start(budget *budgetOwner, clock Clock, startErr error) {
	c.budget, c.clock = budget, clock
	go c.writeFrames()
	go c.closeOnCompletion(budget.workContext)
	go func() {
		defer close(c.done)
		if startErr == nil {
			c.fault = c.run()
		}
		close(c.protocolDone)
		// A clean Done leaves input to the writer, which closes it after the last
		// queued frame; any other end closes now so a stalled writer cannot hold it.
		if c.fault != nil || !c.finished {
			c.closeInput()
		}
		c.closeOutbound()
		<-c.writerDone
		<-c.closerDone
	}()
}

func (c *interactiveInvocation) closeOnCompletion(ctx context.Context) {
	defer close(c.closerDone)
	select {
	case <-ctx.Done():
	case <-c.writerDone:
	}
	c.closeInput()
}

func (c *interactiveInvocation) writeFrames() {
	defer close(c.writerDone)
	for frame := range c.outbound {
		data := append(bytes.Clone(frame.data), '\n')
		if c.validateProject != nil {
			if err := c.validateProject(c.budget.workContext); err != nil {
				c.writeFault = err
				return
			}
		}
		n, err := c.input.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		if err != nil {
			c.writeFault = err
			return
		}
		if frame.messageID != "" {
			select {
			case c.written <- frame.messageID:
			case <-c.protocolDone:
			case <-c.budget.workContext.Done():
				return
			}
		}
	}
}

func (c *interactiveInvocation) Complete() error {
	close(c.chunks)
	if c.records != nil {
		close(c.records)
	}
	<-c.done
	if c.user == nil {
		return c.completionError()
	}
	return errors.Join(c.completionError(), c.flushWritten())
}

func (c *interactiveInvocation) userChannel() bool {
	return c != nil && c.user != nil
}

func (c *interactiveInvocation) completionDiagnostic() string {
	err := c.completionError()
	switch {
	case err == nil:
		return ""
	case c.user == nil:
		return "native protocol did not complete: " + err.Error()
	default:
		return "interactive protocol did not complete: " + err.Error()
	}
}

// Called only after the protocol and writer workers have joined. Native
// completion is independent of whether a concurrent user message reached stdin.
// Keep that delivery unknown; expected pipe closure cannot revoke completion.
// A machine reply is protocol state, so its write failure is never forgiven.
func (c *interactiveInvocation) completionError() error {
	protocolErr := c.protocol.Complete()
	writeErr := c.writeFault
	if c.user != nil && c.finished && protocolErr == nil && (errors.Is(writeErr, os.ErrClosed) || errors.Is(writeErr, io.ErrClosedPipe) || errors.Is(writeErr, syscall.EPIPE)) {
		writeErr = nil
	}
	return errors.Join(c.fault, writeErr, c.closeFault, protocolErr)
}

func (c *interactiveInvocation) run() error {
	frames, err := c.protocol.Start(c.brief)
	c.brief = nil
	if err != nil {
		return err
	}
	if err = c.apply(InteractionUpdate{Outbound: frames}); err != nil {
		return err
	}
	timer := c.newInboxTimer()
	defer func() { timer.Stop() }()
	chunks, records := c.chunks, c.records
	for chunks != nil || records != nil {
		select {
		case <-c.budget.workContext.Done():
			return nil
		case v, ok := <-records:
			if !ok {
				records = nil
				continue
			}
			err = c.consumeRecord(v)
		case data, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			err = c.consume(data)
		case id := <-c.written:
			err = c.recordSent(id)
		case <-timer.C():
			err = c.pollInbox()
			timer = c.newInboxTimer()
		}
		if err != nil {
			return err
		}
	}
	return c.endStream()
}

func (c *interactiveInvocation) consume(data []byte) error {
	for len(data) > 0 {
		line, rest, complete := bytes.Cut(data, []byte{'\n'})
		if len(c.pending)+len(line) > task.MaxControlRecordSize {
			return task.ErrControlRecordTooBig
		}
		c.pending = append(c.pending, line...)
		if !complete {
			return nil
		}
		update, err := c.protocol.Receive(c.pending)
		c.pending = nil
		if err != nil {
			return err
		}
		if err = c.apply(update); err != nil {
			return err
		}
		data = rest
	}
	return nil
}

func (c *interactiveInvocation) endStream() error {
	if len(c.pending) == 0 {
		return nil
	}
	update, err := c.protocol.Receive(c.pending)
	c.pending = nil
	if err != nil {
		return err
	}
	return c.apply(update)
}

func (c *interactiveInvocation) newInboxTimer() Timer {
	if c.user == nil {
		return inactiveTimer{}
	}
	return c.clock.NewTimer(interactionPollInterval)
}

func (c *interactiveInvocation) apply(update InteractionUpdate) error {
	if c.user == nil {
		return c.applyMachine(update)
	}
	// A late native receipt remains in raw stdout, but cannot reopen the sealed
	// interaction boundary or turn uncertain delivery into a new send.
	if c.finished {
		return nil
	}
	for _, event := range update.Events {
		if event.Kind == "ready" && c.budget != nil {
			event.Deadline = c.budget.deadline.UTC().Format(time.RFC3339Nano)
		}
		if err := c.td.RecordInteraction(event); err != nil {
			return err
		}
	}
	for _, frame := range update.Outbound {
		if err := c.queue(interactionFrame{data: frame}); err != nil {
			return err
		}
	}
	if update.Done && !c.finished {
		c.finished = true
		if err := c.flushWritten(); err != nil {
			return err
		}
		if err := c.td.RecordInteraction(task.InteractionEvent{Kind: "closed"}); err != nil {
			return err
		}
		// The native terminal event closes input, never signals a process. The
		// provider must exit naturally; otherwise the original supervisor budget wins.
		c.closeInput()
		return c.closeFault
	}
	return nil
}

// applyMachine validates every record, including those after Done. Events are
// refused before any outbound effect, and Done grants no later outbound.
func (c *interactiveInvocation) applyMachine(update InteractionUpdate) error {
	if len(update.Events) != 0 {
		return errMachineProtocolEvents
	}
	for _, frame := range update.Outbound {
		if err := c.queue(interactionFrame{data: frame}); err != nil {
			return err
		}
	}
	if !update.Done || c.finished {
		return nil
	}
	c.finished = true
	// Done ends the frame queue without blocking behind the writer. The writer
	// drains every earlier frame, then closes input; it never signals a process.
	// The provider must exit naturally; otherwise the original supervisor budget wins.
	c.closeOutbound()
	return nil
}

func (c *interactiveInvocation) closeOutbound() {
	c.outboundOnce.Do(func() { close(c.outbound) })
}

func (c *interactiveInvocation) queue(frame interactionFrame) error {
	if c.finished {
		return task.ErrInteractionClosed
	}
	select {
	case c.outbound <- frame:
		return nil
	case <-c.writerDone:
		return errors.New("native interaction input closed")
	default:
		return errors.New("native interaction output queue exhausted")
	}
}

func (c *interactiveInvocation) deliver() error {
	message, err := c.td.NextInteraction()
	if errors.Is(err, task.ErrInteractionClosed) {
		c.finished = true
		c.closeInput()
		return c.closeFault
	}
	if errors.Is(err, task.ErrInteractionUnavailable) {
		return nil
	}
	if err != nil || message == nil {
		return err
	}
	if err = c.budget.authorize(Options{}, "interaction-delivery-authorized"); err != nil {
		return err
	}
	frame, err := c.user.Encode(*message)
	if err != nil {
		return c.td.RecordInteraction(task.InteractionEvent{Kind: "delivery", Delivery: &task.InteractionDelivery{MessageID: message.ID, State: "rejected", Reason: err.Error()}})
	}
	return c.queue(interactionFrame{data: frame, messageID: message.ID})
}

func (c *interactiveInvocation) recordSent(id string) error {
	state, err := c.td.InteractionState()
	if err != nil {
		return err
	}
	if state.Closed {
		return nil
	}
	for _, delivery := range state.Deliveries {
		if delivery.MessageID == id && delivery.State == "unknown" {
			return c.td.RecordInteraction(task.InteractionEvent{Kind: "delivery", Delivery: &task.InteractionDelivery{MessageID: id, State: "sent"}})
		}
	}
	return nil
}

func (c *interactiveInvocation) closeInput() {
	c.closeOnce.Do(func() { c.closeFault = c.input.Close() })
}

func (c *interactiveInvocation) pollInbox() error {
	if c.finished {
		return nil
	}
	return c.deliver()
}

func (c *interactiveInvocation) flushWritten() error {
	for {
		select {
		case id := <-c.written:
			if err := c.recordSent(id); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (c *interactiveInvocation) consumeRecord(v providerjson.Value) error {
	var update InteractionUpdate
	var err error
	if protocol, ok := c.protocol.(CurrentRecordInteractionProtocol); ok && protocol.UseRecordCapture() {
		update, err = protocol.ReceiveRecord(v)
	} else {
		var data []byte
		data, err = v.Raw(task.MaxControlRecordSize)
		if err == nil {
			update, err = c.protocol.Receive(data)
		}
	}
	if err != nil {
		return err
	}
	return c.apply(update)
}
