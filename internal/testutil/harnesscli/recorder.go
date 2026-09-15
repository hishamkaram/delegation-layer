package harnesscli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const hookCapturePrefix = 4096

var (
	errRecorderDuplicate = errors.New("supervisor event recorder received a duplicate event")
	errWaitHook          = errors.New("supervisor wait hook failure")
	errCaptureHook       = errors.New("supervisor capture hook failure")
)

type processReceipt struct {
	SchemaVersion int      `json:"schema_version"`
	InvocationID  string   `json:"invocation_id"`
	PID           int      `json:"pid"`
	Argv          []string `json:"argv"`
	Entry         string   `json:"entry"`
	Completed     string   `json:"completed,omitempty"`
	ExitCode      int      `json:"exit_code"`
	Failed        bool     `json:"failed"`
	Error         string   `json:"error,omitempty"`
}

type eventEnvelope struct {
	SchemaVersion int                 `json:"schema_version"`
	Kind          string              `json:"kind"`
	InvocationID  string              `json:"invocation_id"`
	Sequence      uint64              `json:"sequence"`
	Name          string              `json:"name,omitempty"`
	PID           int                 `json:"pid"`
	Argv          []string            `json:"argv"`
	At            string              `json:"at"`
	Command       *pueue.CommandEvent `json:"command,omitempty"`
}

type supervisorCompletion struct {
	SchemaVersion int                `json:"schema_version"`
	Kind          string             `json:"kind"`
	InvocationID  string             `json:"invocation_id"`
	CommandID     string             `json:"command_id"`
	PID           int                `json:"pid"`
	ExitCode      int                `json:"exit_code"`
	Argv          []string           `json:"argv"`
	Completed     string             `json:"completed"`
	Failed        bool               `json:"failed"`
	Command       pueue.CommandEvent `json:"command"`
}

type wrapperState struct {
	receipt   processReceipt
	sequence  uint64
	completed bool
}

type supervisorState struct {
	dir       string
	sequence  uint64
	completed bool
}

type eventRecorder struct {
	mu       sync.Mutex
	root     string
	mode     HookMode
	wrapper  *wrapperState
	commands map[string]*supervisorState
	err      error
}

func newEventRecorder(cfg HarnessConfig) *eventRecorder {
	return &eventRecorder{
		root:     cfg.EventsDirectory,
		mode:     cfg.Hooks.Mode,
		commands: make(map[string]*supervisorState),
	}
}

func (r *eventRecorder) begin(argv []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wrapper != nil {
		return errRecorderDuplicate
	}
	if err := os.MkdirAll(r.root, 0o700); err != nil {
		r.recordErrorLocked(err)
		return err
	}
	id, err := task.NewRandomID()
	if err != nil {
		r.recordErrorLocked(err)
		return err
	}
	dir := filepath.Join(r.root, id)
	if err = os.Mkdir(dir, 0o700); err != nil {
		r.recordErrorLocked(err)
		return err
	}
	receipt := processReceipt{
		SchemaVersion: 1,
		InvocationID:  id,
		PID:           os.Getpid(),
		Argv:          append([]string(nil), argv...),
		Entry:         time.Now().UTC().Format(time.RFC3339Nano),
		ExitCode:      -1,
	}
	if err = writeCanonicalExclusive(filepath.Join(dir, "entry.json"), receipt); err != nil {
		r.recordErrorLocked(err)
		return err
	}
	r.wrapper = &wrapperState{receipt: receipt}
	return nil
}

func (r *eventRecorder) executionHooks(cfg HookConfig) execution.Hooks {
	hooks := execution.Hooks{Event: r.executionEvent}
	switch cfg.Mode {
	case HookStartFailed:
		hooks.Start = startFailed
	case HookWaitFailed:
		hooks.Wait = waitFailed
	case HookCaptureFailed:
		hooks.Capture = captureFailed
	case HookStartDelayed:
		hooks.Start = func(cmd *exec.Cmd) error {
			if err := waitForHook(cfg); err != nil {
				return err
			}
			return cmd.Start()
		}
	case HookPublicationDelayed:
		hooks.Event = func(name string) {
			r.executionEvent(name)
			if name == "published" {
				if err := waitForHook(cfg); err != nil {
					r.recordError(err)
				}
			}
		}
	case HookNormal, HookAbandonBeforeSeal:
	}
	return hooks
}

func (r *eventRecorder) executionEvent(name string) {
	abandon := false
	r.mu.Lock()
	if r.wrapper == nil {
		r.mu.Unlock()
		return
	}
	if r.wrapper.completed {
		r.recordErrorLocked(errRecorderDuplicate)
		r.mu.Unlock()
		return
	}
	r.wrapper.sequence++
	event := eventEnvelope{
		SchemaVersion: 1,
		Kind:          "execution",
		InvocationID:  r.wrapper.receipt.InvocationID,
		Sequence:      r.wrapper.sequence,
		Name:          name,
		PID:           r.wrapper.receipt.PID,
		Argv:          append([]string(nil), r.wrapper.receipt.Argv...),
		At:            time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeCanonicalExclusive(filepath.Join(r.root, r.wrapper.receipt.InvocationID, fmt.Sprintf("event-%06d.json", r.wrapper.sequence)), event); err != nil {
		r.recordErrorLocked(err)
	}
	if r.mode == HookAbandonBeforeSeal && name == "completion-observed" {
		if err := r.writeWrapperCompletionLocked(73, "abandon-before-seal"); err != nil {
			r.recordErrorLocked(err)
		}
		abandon = true
	}
	r.mu.Unlock()
	if abandon {
		os.Exit(73)
	}
}

func (r *eventRecorder) supervisorEvent(command pueue.CommandEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wrapper == nil {
		return
	}
	if err := task.ValidateTaskID(command.CommandID); err != nil {
		r.recordErrorLocked(err)
		return
	}
	switch command.Stage {
	case "entry":
		r.recordSupervisorEntryLocked(command)
	case "started", "completed":
		r.recordSupervisorProgressLocked(command)
	default:
		r.recordErrorLocked(fmt.Errorf("unknown supervisor event stage %q", command.Stage))
	}
}

func (r *eventRecorder) recordSupervisorEntryLocked(command pueue.CommandEvent) {
	if _, exists := r.commands[command.CommandID]; exists {
		r.recordErrorLocked(errRecorderDuplicate)
		return
	}
	dir := filepath.Join(r.root, command.CommandID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		r.recordErrorLocked(err)
		return
	}
	event := eventEnvelope{
		SchemaVersion: 1,
		Kind:          "supervisor",
		InvocationID:  command.CommandID,
		Sequence:      0,
		PID:           command.PID,
		Argv:          append([]string(nil), command.Argv...),
		At:            time.Now().UTC().Format(time.RFC3339Nano),
		Command:       commandPointer(command),
	}
	if err := writeCanonicalExclusive(filepath.Join(dir, "entry.json"), event); err != nil {
		r.recordErrorLocked(err)
		return
	}
	r.commands[command.CommandID] = &supervisorState{dir: dir}
}

func (r *eventRecorder) recordSupervisorProgressLocked(command pueue.CommandEvent) {
	state, exists := r.commands[command.CommandID]
	if !exists {
		r.recordErrorLocked(fmt.Errorf("supervisor event %s had no entry", command.CommandID))
		return
	}
	if state.completed {
		r.recordErrorLocked(errRecorderDuplicate)
		return
	}
	if command.Stage == "completed" {
		completion := supervisorCompletion{
			SchemaVersion: 1,
			Kind:          "supervisor",
			InvocationID:  command.CommandID,
			CommandID:     command.CommandID,
			PID:           command.PID,
			ExitCode:      command.ExitCode,
			Argv:          append([]string(nil), command.Argv...),
			Completed:     time.Now().UTC().Format(time.RFC3339Nano),
			Failed:        command.PID <= 0 || command.ExitCode != 0,
			Command:       command,
		}
		if err := writeCanonicalExclusive(filepath.Join(state.dir, "completion.json"), completion); err != nil {
			r.recordErrorLocked(err)
			return
		}
		state.completed = true
		return
	}
	state.sequence++
	event := eventEnvelope{
		SchemaVersion: 1,
		Kind:          "supervisor",
		InvocationID:  command.CommandID,
		Sequence:      state.sequence,
		PID:           command.PID,
		Argv:          append([]string(nil), command.Argv...),
		At:            time.Now().UTC().Format(time.RFC3339Nano),
		Command:       commandPointer(command),
	}
	if err := writeCanonicalExclusive(filepath.Join(state.dir, fmt.Sprintf("event-%06d.json", state.sequence)), event); err != nil {
		r.recordErrorLocked(err)
	}
}

func (r *eventRecorder) finish(code int, errorText string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wrapper == nil {
		return r.err
	}
	if r.wrapper.completed {
		return errors.Join(r.err, errRecorderDuplicate)
	}
	if r.err != nil && errorText == "" {
		errorText = r.err.Error()
	}
	if err := r.writeWrapperCompletionLocked(code, errorText); err != nil {
		r.recordErrorLocked(err)
	}
	return r.err
}

func (r *eventRecorder) writeWrapperCompletionLocked(code int, errorText string) error {
	if r.wrapper.completed {
		return errRecorderDuplicate
	}
	receipt := r.wrapper.receipt
	receipt.Completed = time.Now().UTC().Format(time.RFC3339Nano)
	receipt.ExitCode = code
	receipt.Failed = code != 0 || r.err != nil
	receipt.Error = errorText
	if err := writeCanonicalExclusive(filepath.Join(r.root, receipt.InvocationID, "completion.json"), receipt); err != nil {
		return err
	}
	r.wrapper.receipt = receipt
	r.wrapper.completed = true
	return nil
}

func (r *eventRecorder) recordErrorLocked(err error) {
	if err != nil {
		r.err = errors.Join(r.err, err)
	}
}

func (r *eventRecorder) recordError(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	r.recordErrorLocked(err)
	r.mu.Unlock()
}

func commandPointer(command pueue.CommandEvent) *pueue.CommandEvent {
	copy := command
	copy.Argv = append([]string(nil), command.Argv...)
	return &copy
}

func startFailed(cmd *exec.Cmd) error {
	cmd.Path = filepath.Join(filepath.Dir(cmd.Path), ".provider-fixture-provider-absent")
	return cmd.Start()
}

func waitFailed(cmd *exec.Cmd) error {
	return errors.Join(cmd.Wait(), errWaitHook)
}

func captureFailed(_ string, dst io.Writer, src io.Reader) error {
	_, copyErr := io.Copy(dst, io.LimitReader(src, hookCapturePrefix))
	return errors.Join(copyErr, errCaptureHook)
}

func waitForHook(cfg HookConfig) error {
	if cfg.DelayMS == 0 && cfg.ReleasePath == "" {
		return nil
	}
	if cfg.ReleasePath == "" {
		deadline := time.Now().Add(time.Duration(cfg.DelayMS) * time.Millisecond)
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		<-timer.C
		return nil
	}
	delay := time.Duration(cfg.DelayMS) * time.Millisecond
	if delay == 0 {
		delay = maxHookDelay
	}
	deadline := time.Now().Add(delay)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(cfg.ReleasePath); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
			return nil
		case <-ticker.C:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
}

func writeCanonicalExclusive(path string, value any) error {
	data, err := task.MarshalCanonical(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writeErr := writeAll(f, data)
	return errors.Join(writeErr, f.Sync(), f.Close())
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return fmt.Errorf("invalid event recorder write count %d", n)
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
