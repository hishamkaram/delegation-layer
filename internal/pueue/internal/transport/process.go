package transport

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/protocol"
)

type cappedCapture struct {
	data     []byte
	overflow bool
}

// Write drains even excess output; returning a short write would abandon a pipe.
func (w *cappedCapture) Write(p []byte) (int, error) {
	n := len(p)
	remaining := MaxControlBytes - len(w.data)
	if len(p) > remaining {
		w.overflow = true
		p = p[:remaining]
	}
	w.data = append(w.data, p...)
	return n, nil
}

func StartOwned(cmd *exec.Cmd) *Pending {
	return StartOwnedObserved(cmd, "", nil)
}

func StartOwnedObserved(cmd *exec.Cmd, commandID string, observer func(CommandEvent)) *Pending {
	return StartOwnedCaptured(cmd, commandID, observer, "")
}

// statusVersion selects streaming status projection while preserving the same
// Start/Wait owner and pending-command lifetime as every other control command.
func StartOwnedCaptured(cmd *exec.Cmd, commandID string, observer func(CommandEvent), statusVersion string) *Pending {
	p := NewPending(cmd.Args)
	go func() {
		emitCommand(observer, commandID, "entry", cmd, CommandResult{ExitCode: -1})
		result := exactStartWait(cmd, statusVersion, func(started CommandResult) {
			emitCommand(observer, commandID, "started", cmd, started)
		})
		emitCommand(observer, commandID, "completed", cmd, result)
		p.Complete(result)
	}()
	return p
}

// exactStartWait is the sole Start/Wait implementation for supervisor
// controls. Both the in-process test owner and the one-shot external owner use
// it so output draining, status projection, and completion semantics cannot
// drift.
func exactStartWait(cmd *exec.Cmd, statusVersion string, onStarted func(CommandResult)) CommandResult {
	stdout, stderr := &cappedCapture{}, &cappedCapture{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	var status *statusCapture
	var statusErr error
	if statusVersion != "" {
		status, statusErr = newStatusCapture(statusVersion)
		if statusErr == nil {
			cmd.Stdout = status.writer
		}
	}
	result := CommandResult{ExitCode: -1}
	if statusErr != nil {
		result.Err = statusErr
	} else if err := cmd.Start(); err != nil {
		result.Err = err
	} else {
		result.Started, result.PID = true, cmd.Process.Pid
		if onStarted != nil {
			onStarted(result)
		}
		result.Err = cmd.Wait()
		if cmd.ProcessState != nil {
			result.ExitCode = cmd.ProcessState.ExitCode()
		} else {
			result.Err = errors.Join(result.Err, errors.New("supervisor Wait returned no process state"))
		}
	}
	if status != nil {
		snapshot, captureErr := status.finish()
		result.snapshot = &snapshot
		result.Err = errors.Join(result.Err, captureErr)
	}
	result.Stdout, result.Stderr = stdout.data, stderr.data
	if stdout.overflow || stderr.overflow {
		result.Err = errors.Join(result.Err, ErrControlLimit)
	}
	return result
}

// statusCapture discards task environments as each record is parsed. On any
// parser error it still drains stdout until natural process exit, so an early
// observation or malformed row cannot abandon a child pipe.
type statusCapture struct {
	reader   *os.File
	writer   *os.File
	done     chan struct{}
	snapshot QueueSnapshot
	err      error
}

func newStatusCapture(version string) (*statusCapture, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	capture := &statusCapture{reader: reader, writer: writer, done: make(chan struct{})}
	go func() {
		capture.snapshot, capture.err = protocol.ParseQueueSnapshotStream(reader, version)
		_, drainErr := io.Copy(io.Discard, reader)
		capture.err = errors.Join(capture.err, drainErr, reader.Close())
		close(capture.done)
	}()
	return capture, nil
}

func (c *statusCapture) finish() (QueueSnapshot, error) {
	closeErr := c.writer.Close()
	<-c.done
	return c.snapshot, errors.Join(c.err, closeErr)
}

func emitCommand(observer func(CommandEvent), id, stage string, cmd *exec.Cmd, result CommandResult) {
	if observer != nil {
		observer(CommandEvent{CommandID: id, Stage: stage, Argv: append([]string(nil), cmd.Args...), PID: result.PID, ExitCode: result.ExitCode})
	}
}

func EmitCommand(observer func(CommandEvent), id, stage string, cmd *exec.Cmd, result CommandResult) {
	emitCommand(observer, id, stage, cmd, result)
}

func ObserveCommand(ctx context.Context, p *Pending, timeout time.Duration) (CommandResult, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		result, _ := p.Result()
		return result, result.Err
	case <-ctx.Done():
	case <-timer.C:
	}
	// Prefer a completion already visible at the observation boundary.
	if result, done := p.Result(); done {
		return result, result.Err
	}
	return CommandResult{}, &InFlightError{Pending: p}
}

func PendingFrom(err error) *Pending {
	var inFlight *InFlightError
	if errors.As(err, &inFlight) {
		return inFlight.Pending
	}
	return nil
}

func PendingHandles(err error) []*Pending {
	if err == nil {
		return nil
	}
	seen := make(map[*Pending]struct{})
	var pending []*Pending
	var visit func(error)
	visit = func(current error) {
		if current == nil {
			return
		}
		var inFlight *InFlightError
		if errors.As(current, &inFlight) && inFlight != nil && inFlight.Pending != nil {
			if _, exists := seen[inFlight.Pending]; !exists {
				seen[inFlight.Pending] = struct{}{}
				pending = append(pending, inFlight.Pending)
			}
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				visit(child)
			}
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			visit(wrapped.Unwrap())
		}
	}
	visit(err)
	return pending
}

// AwaitPending preserves process ownership when the caller's finite wait
// expires. The pending handle remains embedded in the returned error so a
// bootstrap owner can keep its serialization lock until Wait completes.
func AwaitPending(ctx context.Context, pending *Pending) (CommandResult, error) {
	if pending == nil {
		return CommandResult{}, nil
	}
	if ctx == nil {
		return CommandResult{}, errors.Join(context.Canceled, &InFlightError{Pending: pending})
	}
	select {
	case <-pending.Done():
		result, _ := pending.Result()
		return result, result.Err
	case <-ctx.Done():
		return CommandResult{}, errors.Join(ctx.Err(), &InFlightError{Pending: pending})
	}
}

// ReapPending keeps command ownership until the process has naturally
// completed and Wait has published its result. Bootstrap callers release
// their lock only after this point.
func ReapPending(pending *Pending) CommandResult {
	if pending == nil {
		return CommandResult{}
	}
	<-pending.Done()
	result, _ := pending.Result()
	return result
}

func RunOwnedCommand(cmd *exec.Cmd, statusVersion string, onStarted func(CommandResult)) CommandResult {
	return exactStartWait(cmd, statusVersion, onStarted)
}
