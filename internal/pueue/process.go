package pueue

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

// InFlightError exposes a command whose caller stopped waiting. Pending retains
// ownership of its eventual Wait result; this error does not establish exit.
type InFlightError struct{ Pending *Pending }

func (e *InFlightError) Error() string { return ErrInFlight.Error() }
func (e *InFlightError) Unwrap() error { return ErrInFlight }

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

func startOwned(cmd *exec.Cmd) *Pending {
	return startOwnedObserved(cmd, "", nil)
}

func startOwnedObserved(cmd *exec.Cmd, commandID string, observer func(CommandEvent)) *Pending {
	p := &Pending{done: make(chan struct{}), args: append([]string(nil), cmd.Args...)}
	go func() {
		stdout, stderr := &cappedCapture{}, &cappedCapture{}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		result := CommandResult{ExitCode: -1}
		emitCommand(observer, commandID, "entry", cmd, result)
		if err := cmd.Start(); err != nil {
			result.Err = err
		} else {
			result.Started, result.PID = true, cmd.Process.Pid
			emitCommand(observer, commandID, "started", cmd, result)
			result.Err = cmd.Wait()
			if cmd.ProcessState != nil {
				result.ExitCode = cmd.ProcessState.ExitCode()
			} else {
				result.Err = errors.Join(result.Err, errors.New("supervisor Wait returned no process state"))
			}
		}
		result.Stdout, result.Stderr = stdout.data, stderr.data
		if stdout.overflow || stderr.overflow {
			result.Err = errors.Join(result.Err, ErrControlLimit)
		}
		emitCommand(observer, commandID, "completed", cmd, result)
		p.result = result
		close(p.done)
	}()
	return p
}

func emitCommand(observer func(CommandEvent), id, stage string, cmd *exec.Cmd, result CommandResult) {
	if observer != nil {
		observer(CommandEvent{CommandID: id, Stage: stage, Argv: append([]string(nil), cmd.Args...), PID: result.PID, ExitCode: result.ExitCode})
	}
}

func observeCommand(ctx context.Context, p *Pending, timeout time.Duration) (CommandResult, error) {
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

func pendingFrom(err error) *Pending {
	var inFlight *InFlightError
	if errors.As(err, &inFlight) {
		return inFlight.Pending
	}
	return nil
}

// awaitPending preserves process ownership when the caller's finite wait
// expires. The pending handle remains embedded in the returned error so a
// bootstrap owner can keep its serialization lock until Wait completes.
func awaitPending(ctx context.Context, pending *Pending) (CommandResult, error) {
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

// reapPending keeps command ownership until the process has naturally
// completed and Wait has published its result. Bootstrap callers release
// their lock only after this point.
func reapPending(pending *Pending) CommandResult {
	if pending == nil {
		return CommandResult{}
	}
	<-pending.Done()
	result, _ := pending.Result()
	return result
}
