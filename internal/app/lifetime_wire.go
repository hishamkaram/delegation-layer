package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const lifetimeProtocolVersion = 1

const lifetimeControlTimeout = 10 * time.Second

const (
	lifetimeMessageHello   = "hello"
	lifetimeMessageBegin   = "begin"
	lifetimeMessageStarted = "started"
	lifetimeMessageFailed  = "failed"
	lifetimeMessageFinish  = "finish"
	lifetimeMessageStop    = "stop"
	lifetimeMessageClose   = "close"
)

type lifetimeMessage struct {
	Version  int    `json:"version"`
	Type     string `json:"type"`
	TaskID   string `json:"task_id"`
	Sequence uint64 `json:"sequence,omitempty"`
	PID      int    `json:"pid,omitempty"`
}

type lifetimeReply struct {
	Version  int    `json:"version"`
	Type     string `json:"type"`
	Sequence uint64 `json:"sequence,omitempty"`
	Accepted bool   `json:"accepted"`
	Code     string `json:"code,omitempty"`
}

type lifetimeCodec struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
	closed bool
}

func newLifetimeCodec(conn io.ReadWriteCloser) (*lifetimeCodec, error) {
	if conn == nil {
		return nil, task.ErrEvidenceFault
	}
	network, ok := conn.(net.Conn)
	if !ok {
		return nil, task.ErrEvidenceFault
	}
	return &lifetimeCodec{conn: network, reader: bufio.NewReaderSize(network, int(task.MaxControlRecordSize)+1)}, nil
}

func (c *lifetimeCodec) call(message lifetimeMessage) error {
	ctx, cancel := context.WithTimeout(context.Background(), lifetimeControlTimeout)
	defer cancel()
	return c.callContext(ctx, message)
}

func (c *lifetimeCodec) callContext(ctx context.Context, message lifetimeMessage) error {
	if c == nil {
		return task.ErrEvidenceFault
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return osClosedError()
	}
	if err := validateLifetimeMessage(message); err != nil {
		return err
	}
	clear, err := c.setDeadline(ctx)
	if err != nil {
		return err
	}
	defer clear()
	if err = c.writeFrame(message); err != nil {
		return err
	}
	var reply lifetimeReply
	if err = c.readFrame(&reply); err != nil {
		return err
	}
	if err := validateLifetimeReply(reply, message); err != nil {
		return err
	}
	if !reply.Accepted {
		return errors.New("task lifetime request refused: " + reply.Code)
	}
	return nil
}

func (c *lifetimeCodec) receive() (lifetimeMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lifetimeControlTimeout)
	defer cancel()
	return c.receiveContext(ctx)
}

func (c *lifetimeCodec) receiveContext(ctx context.Context) (lifetimeMessage, error) {
	if c == nil {
		return lifetimeMessage{}, task.ErrEvidenceFault
	}
	clear, err := c.setDeadline(ctx)
	if err != nil {
		return lifetimeMessage{}, err
	}
	defer clear()
	var message lifetimeMessage
	if err = c.readFrame(&message); err != nil {
		return lifetimeMessage{}, err
	}
	return message, validateLifetimeMessage(message)
}

func (c *lifetimeCodec) reply(message lifetimeMessage, accepted bool, code string) error {
	ctx, cancel := context.WithTimeout(context.Background(), lifetimeControlTimeout)
	defer cancel()
	return c.replyContext(ctx, message, accepted, code)
}

func (c *lifetimeCodec) replyContext(ctx context.Context, message lifetimeMessage, accepted bool, code string) error {
	if c == nil {
		return task.ErrEvidenceFault
	}
	clear, err := c.setDeadline(ctx)
	if err != nil {
		return err
	}
	defer clear()
	reply := lifetimeReply{Version: lifetimeProtocolVersion, Type: message.Type, Sequence: message.Sequence, Accepted: accepted, Code: code}
	if accepted {
		reply.Code = ""
	}
	return c.writeFrame(reply)
}

func (c *lifetimeCodec) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

func (c *lifetimeCodec) setDeadline(ctx context.Context) (func(), error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return nil, task.ErrEvidenceFault
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	return func() { clearLifetimeDeadline(c.conn) }, nil
}

func clearLifetimeDeadline(conn net.Conn) {
	if conn == nil {
		return
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return
	}
}

func (c *lifetimeCodec) writeFrame(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if int64(len(data)+1) > task.MaxControlRecordSize {
		return task.ErrEvidenceFault
	}
	data = append(data, '\n')
	for len(data) > 0 {
		written, writeErr := c.conn.Write(data)
		if writeErr != nil {
			return writeErr
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func (c *lifetimeCodec) readFrame(value any) error {
	data, err := c.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return task.ErrEvidenceFault
	}
	if err != nil {
		return err
	}
	if len(data) == 0 || int64(len(data)) > task.MaxControlRecordSize {
		return task.ErrEvidenceFault
	}
	return task.DecodeStrict(data[:len(data)-1], value)
}

func validateLifetimeMessage(message lifetimeMessage) error {
	if message.Version != lifetimeProtocolVersion || task.ValidateTaskID(message.TaskID) != nil {
		return task.ErrEvidenceFault
	}
	switch message.Type {
	case lifetimeMessageHello, lifetimeMessageStop, lifetimeMessageClose:
		if message.Sequence != 0 || message.PID != 0 {
			return task.ErrEvidenceFault
		}
	case lifetimeMessageBegin, lifetimeMessageFailed, lifetimeMessageFinish:
		if message.Sequence == 0 || message.PID != 0 {
			return task.ErrEvidenceFault
		}
	case lifetimeMessageStarted:
		if message.Sequence == 0 || message.PID <= 0 {
			return task.ErrEvidenceFault
		}
	default:
		return task.ErrEvidenceFault
	}
	return nil
}

func validateLifetimeReply(reply lifetimeReply, request lifetimeMessage) error {
	if reply.Version != lifetimeProtocolVersion || reply.Type != request.Type || reply.Sequence != request.Sequence {
		return task.ErrEvidenceFault
	}
	if reply.Accepted == (reply.Code != "") {
		return task.ErrEvidenceFault
	}
	return nil
}

func osClosedError() error {
	return errors.New("task lifetime control is closed")
}

type lifetimeCommandScope interface {
	Configure(*exec.Cmd) error
	ObserveStarted(int) (lifetime.LaunchRef, error)
	Terminate(context.Context, []lifetime.LaunchRef) error
	EmptyContext(context.Context, []lifetime.LaunchRef) (bool, error)
	Release() error
}

type lifetimeWorkerClient struct {
	codec             *lifetimeCodec
	scope             lifetimeCommandScope
	launchGateFactory lifetimeCommandLaunchGateFactory
	taskID            string
	deadline          time.Time
	sequence          uint64
	starting          bool
	activePID         int
	terminal          bool
	controlErr        error
	mu                sync.Mutex
}

func newLifetimeWorkerClient(conn io.ReadWriteCloser, scope lifetimeCommandScope, taskID string, deadline time.Time, launchGateFactory lifetimeCommandLaunchGateFactory) (*lifetimeWorkerClient, error) {
	if scope == nil || launchGateFactory == nil || task.ValidateTaskID(taskID) != nil || deadline.IsZero() {
		return nil, task.ErrEvidenceFault
	}
	codec, err := newLifetimeCodec(conn)
	if err != nil {
		return nil, err
	}
	client := &lifetimeWorkerClient{codec: codec, scope: scope, launchGateFactory: launchGateFactory, taskID: taskID, deadline: deadline}
	if err = client.call(context.Background(), client.message(lifetimeMessageHello, 0, 0)); err != nil {
		return nil, errors.Join(err, codec.Close())
	}
	return client, nil
}

func (c *lifetimeWorkerClient) StartCommand(cmd *exec.Cmd, start func() error) error {
	if c == nil || cmd == nil || start == nil {
		return task.ErrEvidenceFault
	}
	c.mu.Lock()
	if c.starting || c.activePID != 0 || c.terminal {
		c.mu.Unlock()
		return task.ErrEvidenceFault
	}
	if err := c.scope.Configure(cmd); err != nil {
		c.mu.Unlock()
		return err
	}
	gate, err := c.launchGateFactory(cmd)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	c.sequence++
	sequence := c.sequence
	if err := c.call(context.Background(), c.message(lifetimeMessageBegin, sequence, 0)); err != nil {
		c.mu.Unlock()
		return errors.Join(err, gate.abort())
	}
	c.starting = true
	c.mu.Unlock()

	startErr := start()
	gateStartErr := gate.started(startErr)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.completeStartLocked(cmd, gate, sequence, startErr, gateStartErr)
}

// completeStartLocked commits the native Start result while c.mu is held.
// Execution retains the only Wait whenever native process creation succeeded.
func (c *lifetimeWorkerClient) completeStartLocked(cmd *exec.Cmd, gate lifetimeCommandLaunchGate, sequence uint64, startErr, gateStartErr error) error {
	c.starting = false
	if startErr != nil {
		if c.terminal {
			return errors.Join(startErr, gateStartErr, gate.abort())
		}
		return errors.Join(startErr, gateStartErr, c.call(context.Background(), c.message(lifetimeMessageFailed, sequence, 0)))
	}
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		abortErr := gate.abort()
		if c.terminal {
			return errors.Join(task.ErrEvidenceFault, gateStartErr, abortErr)
		}
		return errors.Join(task.ErrEvidenceFault, gateStartErr, abortErr,
			c.call(context.Background(), c.message(lifetimeMessageFailed, sequence, 0)))
	}
	c.activePID = cmd.Process.Pid
	if c.terminal {
		// Termination fenced the guardian while Start was in progress. Keep the
		// successful native Start owned by execution so it performs the one Wait,
		// and close the local gate without sending a message to a terminal guardian.
		c.controlErr = errors.Join(c.controlErr, gateStartErr, gate.abort())
		return nil
	}
	if err := c.call(context.Background(), c.message(lifetimeMessageStarted, sequence, c.activePID)); err != nil {
		// Start succeeded, so execution still owns the command's single Wait.
		// The launch gate keeps the provider from executing while the worker
		// cleans the exact unacknowledged wrapper scope. Retain the fault until
		// FinishCommand runs after execution's Wait.
		c.controlErr = errors.Join(c.controlErr, gateStartErr, err, c.cleanupUnacknowledgedStart(c.activePID), gate.abort())
		return nil
	}
	c.controlErr = errors.Join(c.controlErr, gateStartErr, gate.release())
	return nil
}

func (c *lifetimeWorkerClient) cleanupUnacknowledgedStart(pid int) error {
	launchRef, err := c.scope.ObserveStarted(pid)
	if err != nil {
		return fmt.Errorf("observe unacknowledged lifetime start: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultLifetimeGuardianControlTimeout)
	defer cancel()
	refs := []lifetime.LaunchRef{launchRef}
	if err = c.scope.Terminate(ctx, refs); err != nil {
		empty, emptyErr := c.scope.EmptyContext(ctx, refs)
		if emptyErr == nil && empty {
			return nil
		}
		return errors.Join(fmt.Errorf("terminate unacknowledged lifetime start: %w", err), emptyErr)
	}
	empty, err := c.scope.EmptyContext(ctx, refs)
	if err != nil {
		return fmt.Errorf("verify unacknowledged lifetime start: %w", err)
	}
	if !empty {
		return lifetime.ErrNotEmpty
	}
	return nil
}

func (c *lifetimeWorkerClient) FinishCommand(ctx context.Context, cmd *exec.Cmd) error {
	if c == nil || cmd == nil {
		return task.ErrEvidenceFault
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activePID == 0 || cmd.Process == nil || cmd.Process.Pid != c.activePID {
		return task.ErrIdentityMismatch
	}
	if c.controlErr != nil {
		c.activePID = 0
		return c.controlErr
	}
	if c.terminal {
		c.activePID = 0
		return nil
	}
	err := c.controlCall(ctx, c.message(lifetimeMessageFinish, c.sequence, 0))
	c.activePID = 0
	c.controlErr = errors.Join(c.controlErr, err)
	return err
}

func (c *lifetimeWorkerClient) Terminate(ctx context.Context) error {
	if c == nil {
		return task.ErrEvidenceFault
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminal {
		return nil
	}
	err := c.controlCall(ctx, c.message(lifetimeMessageStop, 0, 0))
	if err == nil {
		c.terminal = true
	}
	return err
}

func (c *lifetimeWorkerClient) CloseTask(ctx context.Context) error {
	if c == nil {
		return task.ErrEvidenceFault
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.starting || c.activePID != 0 {
		return task.ErrEvidenceFault
	}
	if c.controlErr != nil {
		return c.controlErr
	}
	if c.terminal {
		return nil
	}
	err := c.controlCall(ctx, c.message(lifetimeMessageClose, 0, 0))
	if err == nil {
		c.terminal = true
	}
	return err
}

func (c *lifetimeWorkerClient) Close() error {
	if c == nil {
		return nil
	}
	return errors.Join(c.codec.Close(), releaseLifetimeScopeUncanceled(context.Background(), c.scope))
}

func (c *lifetimeWorkerClient) message(kind string, sequence uint64, pid int) lifetimeMessage {
	return lifetimeMessage{Version: lifetimeProtocolVersion, Type: kind, TaskID: c.taskID, Sequence: sequence, PID: pid}
}

func (c *lifetimeWorkerClient) call(parent context.Context, message lifetimeMessage) error {
	if c == nil || c.codec == nil || c.deadline.IsZero() || parent == nil {
		return task.ErrEvidenceFault
	}
	controlCtx, cancelControl := context.WithTimeout(parent, lifetimeControlTimeout)
	defer cancelControl()
	ctx, cancelDeadline := context.WithDeadline(controlCtx, c.deadline)
	defer cancelDeadline()
	return c.codec.callContext(ctx, message)
}

// controlCall gives terminal control messages their own bounded lifetime.
// The immutable task deadline gates new provider work; it must not prevent the
// already-started worker from asking the guardian to stop or close that work.
func (c *lifetimeWorkerClient) controlCall(parent context.Context, message lifetimeMessage) error {
	if c == nil || c.codec == nil || parent == nil {
		return task.ErrEvidenceFault
	}
	ctx, cancel := context.WithTimeout(parent, lifetimeControlTimeout)
	defer cancel()
	return c.codec.callContext(ctx, message)
}
