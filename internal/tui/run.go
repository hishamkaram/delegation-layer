package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/terminal"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type Options struct {
	CheckContext       func(context.Context) error
	Close              func() error
	Root, Cwd, Version string
	Environment        []string
	Input              *os.File
	Output, Error      io.Writer
	Sections           []ui.Section
	Start              func(*session.Tracker, func(tea.Msg)) error
}

// HoldHangupUntilExit is called only by process entrypoints before a TUI run.
// Its additive subscription survives Run's temporary watcher so a late kernel
// HUP cannot restore the default action during final cleanup. The process owns
// this channel until os.Exit; reusable Run/Launch never install this guard.
func HoldHangupUntilExit(args []string) {
	parsed, err := app.ParseArguments(args)
	if err != nil || parsed.Command != "tui" {
		return
	}
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
}

// Run owns the terminal and signals. Every infrastructure/task operation keeps
// its original owner. Only this process exiting ends an unresolved UI effect.
func Run(options Options) int {
	if options.Error == nil {
		options.Error = io.Discard
	}
	descriptor, ok := options.Output.(interface{ Fd() uintptr })
	if options.Input == nil || !ok || !term.IsTerminal(options.Input.Fd()) || !term.IsTerminal(descriptor.Fd()) {
		return reportRunError(options.Error, "tui requires terminal stdin and stdout; use the ordinary commands for scripts", 2)
	}
	if len(options.Sections) == 0 {
		return reportRunError(options.Error, "terminal workbench has no views", 1)
	}
	tracker := session.New(context.Background())
	m := newModel(options.Sections, options.Root, options.Cwd, options.Version, tracker, nil)
	m.onStart = options.Start
	m.checkContext = options.CheckContext
	p := tea.NewProgram(m, tea.WithInput(options.Input), tea.WithOutput(options.Output), tea.WithEnvironment(options.Environment), tea.WithoutSignalHandler())
	m.publish = p.Send
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	done, joined := make(chan struct{}), make(chan os.Signal, 1)
	go watchSignals(signals, done, joined, p.Send)
	_, runErr := p.Run()
	signal.Stop(signals)
	close(done)
	evidence := shutdownEvidence{Signal: <-joined}
	if evidence.Signal != syscall.SIGHUP && inputHangupError(runErr, options.Input) {
		evidence.InputHangup, evidence.ObservationError = pollInputHangup(options.Input, unix.Poll)
	}
	tracker.Stop()
	return finishRun(options, m, tracker, runErr, evidence)
}

// Run owns the immutable shutdown snapshot; the model only renders signal
// effects. Native terminal hangup does not imply a Go notification was delivered.
type shutdownEvidence struct {
	Signal           os.Signal
	InputHangup      bool
	ObservationError error
}

func finishRun(options Options, m *model, tracker *session.Tracker, runErr error, evidence shutdownEvidence) int {
	// Renderer cancellation has already restored the terminal. Give cooperative
	// readers a bounded join; application Effects receive no new cancellation.
	joinContext, cancelJoin := context.WithTimeout(context.Background(), 2*time.Second)
	joinErr := tracker.JoinReads(joinContext)
	cancelJoin()
	state := tracker.Snapshot()
	code, runErr := evidence.exitDecision(m.exitCode, runErr, options.Input)
	if len(state.Pending) == 0 && options.Close != nil {
		if err := options.Close(); err != nil {
			code = reportRunError(options.Error, terminal.Safe(err.Error()), 1)
		}
	}
	if len(state.Pending) > 0 && code == 0 {
		code = 3
	}
	if code == 3 && len(state.Pending) == 0 {
		code = 0
	}
	if err := m.writeShutdownState(options.Error, tracker, state, joinErr); err != nil {
		return 1
	}
	if runErr != nil {
		return reportRunError(options.Error, terminal.Safe(runErr.Error()), code)
	}
	return code
}

func (evidence shutdownEvidence) exitDecision(code int, runErr error, input *os.File) (int, error) {
	// Explicit f+Enter retains the user's force choice. Signal grace expiry
	// uses its signal code instead; independent faults below still win.
	if code != 3 {
		if evidence.Signal != nil {
			code = signalExitCode(evidence.Signal)
		} else if evidence.InputHangup {
			code = 129
		}
	}
	if evidence.ObservationError == nil && (evidence.Signal == syscall.SIGHUP || evidence.InputHangup) && inputHangupError(runErr, input) {
		// HUP or verified terminal loss accounts for the expected read failure
		// without depending on a model update after Bubble Tea has stopped.
		runErr = nil
	}
	runErr = errors.Join(runErr, evidence.ObservationError)
	if runErr != nil && !task.AllErrorCauses(runErr, func(cause error) bool { return errors.Is(cause, tea.ErrInterrupted) }) {
		code = 1
	}
	return code, runErr
}

// pollInputHangup observes only Run's already-validated, borrowed input. The
// concrete poll edge lets faults be verified without closing or reusing its fd.
func pollInputHangup(input *os.File, poll func([]unix.PollFd, int) (int, error)) (bool, error) {
	if input == nil {
		return false, os.ErrInvalid
	}
	descriptor := input.Fd()
	if descriptor > math.MaxInt32 {
		return false, os.ErrInvalid
	}
	fds := []unix.PollFd{{Fd: int32(descriptor), Events: unix.POLLIN}}
	count, err := poll(fds, 0)
	runtime.KeepAlive(input)
	if err != nil {
		return false, fmt.Errorf("observe terminal input hangup: %w", err)
	}
	if fds[0].Revents&unix.POLLNVAL != 0 {
		return false, fmt.Errorf("observe terminal input hangup: %w", unix.EBADF)
	}
	return count > 0 && fds[0].Revents&unix.POLLHUP != 0, nil
}

func inputHangupError(err error, input *os.File) bool {
	if err == nil || input == nil {
		return false
	}
	var read *os.PathError
	if !errors.As(err, &read) || read == nil || read.Op != "read" || read.Path != input.Name() {
		return false
	}
	if !task.AllErrorCauses(read.Err, func(cause error) bool { return errors.Is(cause, syscall.EIO) }) {
		return false
	}
	return onlyHangupCauses(err, read)
}

// onlyHangupCauses preserves every independent error joined to the known read
// failure. Bubble Tea wraps that exact cause together with ErrProgramKilled.
func onlyHangupCauses(err error, read *os.PathError) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyHangupCauses(child, read) {
				return false
			}
		}
		return true
	}
	cause := errors.Unwrap(err)
	// Stop at the node owning this read, rather than an outer wrapper that
	// could also enclose an independent joined failure.
	if errors.Is(err, read) && !errors.Is(cause, read) {
		return true
	}
	if cause != nil {
		return onlyHangupCauses(cause, read)
	}
	return errors.Is(err, tea.ErrProgramKilled)
}

func (m *model) writeShutdownState(output io.Writer, tracker *session.Tracker, state session.State, joinErr error) error {
	if err := writeLateCompletions(output, state.Completed, tracker, &m.restartRequired); err != nil {
		return err
	}
	if err := writeReconciliation(output, state.Pending); err != nil {
		return err
	}
	if joinErr != nil {
		if _, err := fmt.Fprintln(output, "Observation shutdown did not finish within its grace; process exit will release remaining session handles."); err != nil {
			return err
		}
	}
	if m.restartRequired {
		if _, err := fmt.Fprintln(output, "Restart required: this root retained cleanup custody. Do not force-close or replay the failed operation."); err != nil {
			return err
		}
	}
	return nil
}

func reportRunError(output io.Writer, text string, code int) int {
	if _, err := fmt.Fprintln(output, "delegate: "+text); err != nil {
		return 1
	}
	return code
}

func watchSignals(signals <-chan os.Signal, done <-chan struct{}, joined chan<- os.Signal, publish func(tea.Msg)) {
	// Only this watcher writes signal evidence. Run reads the final snapshot
	// after Stop has quiesced delivery and this goroutine has joined.
	var receivedSignal os.Signal
	defer func() { joined <- receivedSignal; close(joined) }()
	var grace *time.Timer
	var expiry <-chan time.Time
	code := 0
	defer func() {
		if grace != nil {
			grace.Stop()
		}
	}()
	for {
		select {
		case <-done:
			receivedSignal = drainStoppedSignals(signals, receivedSignal)
			return
		case received, open := <-signals:
			if !open {
				return
			}
			receivedSignal = received
			next := signalExitCode(received)
			if code != 0 {
				publish(signalMsg{code: next, force: true})
				continue
			}
			code = next
			publish(signalMsg{code: code})
			grace = time.NewTimer(30 * time.Second)
			expiry = grace.C
		case <-expiry:
			publish(signalMsg{code: code, force: true})
			expiry = nil
		}
	}
}

// Stop has quiesced delivery before done closes. Retain the last accepted
// notification even when the renderer ended before its watcher could select it.
func drainStoppedSignals(signals <-chan os.Signal, received os.Signal) os.Signal {
	for {
		select {
		case value, open := <-signals:
			if !open {
				return received
			}
			received = value
		default:
			return received
		}
	}
}

func signalExitCode(value os.Signal) int {
	switch value {
	case os.Interrupt:
		return 130
	case syscall.SIGTERM:
		return 143
	case syscall.SIGHUP:
		return 129
	default:
		return 1
	}
}

func writeReconciliation(output io.Writer, pending []session.Operation) error {
	for _, op := range pending {
		if op.Kind == session.Read {
			if _, err := fmt.Fprintf(output, "Cancelled observation still closing: %s.\n", terminal.Safe(op.Reference.Description)); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(output, "Unresolved: %s. Do not automatically retry.\n", terminal.Safe(op.Reference.Description)); err != nil {
			return err
		}
		if len(op.Reference.Command) > 0 {
			if _, err := fmt.Fprintln(output, "Inspect: "+quotedCommand(op.Reference.Command)); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeLateCompletions(output io.Writer, completions []session.Completion, tracker *session.Tracker, restart *bool) error {
	for _, completion := range completions {
		if err := writeCompletedReferences(output, []session.Reference{completion.Reference}); err != nil {
			return err
		}
		*restart = *restart || completion.RestartRequired
		if completion.Err != nil {
			text := terminal.Safe(completion.Err.Error())
			if len(text) > 2048 {
				text = text[:2048] + "…"
			}
			if _, err := fmt.Fprintln(output, "Operation error: "+text); err != nil {
				return err
			}
		}
		tracker.Acknowledge(completion.ID)
	}
	return nil
}

func quotedCommand(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		if terminal.Safe(arg) != arg {
			return "[reference contains terminal controls; inspect the recorded context explicitly]"
		}
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " ")
}

func writeCompletedReferences(output io.Writer, references []session.Reference) error {
	for _, ref := range references {
		if _, err := fmt.Fprintln(output, "Operation returned while closing: "+terminal.Safe(ref.Description)); err != nil {
			return err
		}
		if len(ref.Command) > 0 {
			if _, err := fmt.Fprintln(output, "Inspect: "+quotedCommand(ref.Command)); err != nil {
				return err
			}
		}
	}
	return nil
}
