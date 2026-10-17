package guardian

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type CommandScope interface {
	Configure(*exec.Cmd) error
	ObserveStarted(int) (lifetime.LaunchRef, error)
	Terminate(context.Context, []lifetime.LaunchRef) error
	EmptyContext(context.Context, []lifetime.LaunchRef) (bool, error)
	Release() error
}

// WorkerClient supplies execution's lifetime port over the private guardian
// protocol. Execution retains ownership of command Start and the exact Wait.
type WorkerClient struct {
	codec             *lifetimeCodec
	scope             CommandScope
	launchGateFactory LaunchGateFactory
	taskID            string
	deadline          time.Time
	sequence          uint64
	starting          bool
	activePID         int
	terminal          bool
	controlErr        error
	mu                sync.Mutex
}

// NewWorkerClient completes the worker hello before permitting command launches.
func NewWorkerClient(conn io.ReadWriteCloser, scope CommandScope, taskID string, deadline time.Time, launchGateFactory LaunchGateFactory) (*WorkerClient, error) {
	if scope == nil || launchGateFactory == nil || task.ValidateTaskID(taskID) != nil || deadline.IsZero() {
		return nil, task.ErrEvidenceFault
	}
	codec, err := newLifetimeCodec(conn)
	if err != nil {
		return nil, err
	}
	client := &WorkerClient{codec: codec, scope: scope, launchGateFactory: launchGateFactory, taskID: taskID, deadline: deadline}
	if err = client.call(context.Background(), client.message(lifetimeMessageHello, 0, 0)); err != nil {
		return nil, errors.Join(err, codec.Close())
	}
	return client, nil
}

func (c *WorkerClient) StartCommand(cmd *exec.Cmd, start func() error) error {
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
		return errors.Join(err, gate.Abort())
	}
	c.starting = true
	c.mu.Unlock()

	startErr := start()
	gateStartErr := gate.Started(startErr)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.completeStartLocked(cmd, gate, sequence, startErr, gateStartErr)
}

// completeStartLocked commits the native Start result while c.mu is held.
// Execution retains the only Wait whenever native process creation succeeded.
func (c *WorkerClient) completeStartLocked(cmd *exec.Cmd, gate LaunchGate, sequence uint64, startErr, gateStartErr error) error {
	c.starting = false
	if startErr != nil {
		if c.terminal {
			return errors.Join(startErr, gateStartErr, gate.Abort())
		}
		return errors.Join(startErr, gateStartErr, c.call(context.Background(), c.message(lifetimeMessageFailed, sequence, 0)))
	}
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		abortErr := gate.Abort()
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
		c.controlErr = errors.Join(c.controlErr, gateStartErr, gate.Abort())
		return nil
	}
	if err := c.call(context.Background(), c.message(lifetimeMessageStarted, sequence, c.activePID)); err != nil {
		// Start succeeded, so execution still owns the command's single Wait.
		// The launch gate keeps the provider from executing while the worker
		// cleans the exact unacknowledged wrapper scope. Retain the fault until
		// FinishCommand runs after execution's Wait.
		c.controlErr = errors.Join(c.controlErr, gateStartErr, err, c.cleanupUnacknowledgedStart(c.activePID), gate.Abort())
		return nil
	}
	c.controlErr = errors.Join(c.controlErr, gateStartErr, gate.Release())
	return nil
}

func (c *WorkerClient) cleanupUnacknowledgedStart(pid int) error {
	launchRef, err := c.scope.ObserveStarted(pid)
	if err != nil {
		return fmt.Errorf("observe unacknowledged lifetime start: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultLifetimeGuardianCleanupTimeout)
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

func (c *WorkerClient) FinishCommand(ctx context.Context, cmd *exec.Cmd) error {
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
	err := c.call(ctx, c.message(lifetimeMessageFinish, c.sequence, 0))
	c.activePID = 0
	c.controlErr = errors.Join(c.controlErr, err)
	return err
}

func (c *WorkerClient) Terminate(ctx context.Context) error {
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

func (c *WorkerClient) CloseTask(ctx context.Context) error {
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
	err := c.call(ctx, c.message(lifetimeMessageClose, 0, 0))
	if err == nil {
		c.terminal = true
	}
	return err
}

func (c *WorkerClient) Close() error {
	if c == nil {
		return nil
	}
	return errors.Join(c.codec.Close(), ReleaseScopeUncanceled(context.Background(), c.scope))
}

func (c *WorkerClient) message(kind string, sequence uint64, pid int) lifetimeMessage {
	return lifetimeMessage{Version: lifetimeProtocolVersion, Type: kind, TaskID: c.taskID, Sequence: sequence, PID: pid}
}

// call keeps hello, native command transitions, and normal completion under
// the immutable task or inspection deadline. controlCall owns the finite
// stop fallback after work can no longer continue.
func (c *WorkerClient) call(parent context.Context, message lifetimeMessage) error {
	if c == nil || c.codec == nil || c.deadline.IsZero() || parent == nil {
		return task.ErrEvidenceFault
	}
	ctx, cancel := context.WithDeadline(parent, c.deadline)
	defer cancel()
	return c.codec.callContext(ctx, message)
}

// controlCall gives stop messages their own bounded lifetime.
// The immutable task deadline gates new provider work; it must not prevent the
// already-started worker from asking the guardian to stop that work.
func (c *WorkerClient) controlCall(parent context.Context, message lifetimeMessage) error {
	if c == nil || c.codec == nil || parent == nil {
		return task.ErrEvidenceFault
	}
	ctx, cancel := context.WithTimeout(parent, lifetimeControlTimeout)
	defer cancel()
	return c.codec.callContext(ctx, message)
}
