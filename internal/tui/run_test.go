package tui

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/sys/unix"
)

func terminalInputError() error {
	return &os.PathError{Op: "read", Path: os.Stdin.Name(), Err: syscall.EIO}
}

func TestFinishRunPreservesReceivedHangupAcrossTerminalReadError(t *testing.T) {
	for _, modelCode := range []int{0, 129} {
		t.Run(fmt.Sprintf("model-code-%d", modelCode), func(t *testing.T) {
			m := fixtureModel(t)
			m.exitCode = modelCode
			var output bytes.Buffer
			closed := false
			options := Options{Input: os.Stdin, Error: &output, Close: func() error { closed = true; return nil }}
			runErr := fmt.Errorf("%w: %w", tea.ErrProgramKilled, terminalInputError())
			if code := finishRun(options, m, m.tracker, runErr, shutdownEvidence{Signal: syscall.SIGHUP}); code != 129 || output.Len() != 0 || !closed {
				t.Fatalf("terminal hangup lost its actual signal: code%d closed%t diagnostic%q", code, closed, output.String())
			}
		})
	}
}

func TestFinishRunKeepsIndependentTerminalFailures(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		signal os.Signal
		err    error
	}{
		{name: "no observed signal", err: fmt.Errorf("%w: %w", tea.ErrProgramKilled, terminalInputError())},
		{name: "different signal", signal: syscall.SIGTERM, err: terminalInputError()},
		{name: "unrelated fault", signal: syscall.SIGHUP, err: errors.New("unrelated renderer fault")},
		{name: "joined independent fault", signal: syscall.SIGHUP, err: errors.Join(tea.ErrProgramKilled, terminalInputError(), errors.New("independent failure"))},
		{name: "joined input fault", signal: syscall.SIGHUP, err: &os.PathError{Op: "read", Path: os.Stdin.Name(), Err: errors.Join(syscall.EIO, errors.New("independent read failure"))}},
		{name: "different operation", signal: syscall.SIGHUP, err: &os.PathError{Op: "write", Path: os.Stdin.Name(), Err: syscall.EIO}},
		{name: "different input", signal: syscall.SIGHUP, err: &os.PathError{Op: "read", Path: "/another-input", Err: syscall.EIO}},
		{name: "killed without input error", signal: syscall.SIGHUP, err: tea.ErrProgramKilled},
		{name: "interrupted with independent fault", err: errors.Join(tea.ErrInterrupted, errors.New("independent I/O failure"))},
		{name: "hangup interrupted with independent fault", signal: syscall.SIGHUP, err: errors.Join(tea.ErrInterrupted, errors.New("independent I/O failure"))},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			m := fixtureModel(t)
			var output bytes.Buffer
			if code := finishRun(Options{Input: os.Stdin, Error: &output}, m, m.tracker, fixture.err, shutdownEvidence{Signal: fixture.signal}); code != 1 || output.Len() == 0 {
				t.Fatalf("independent failure was hidden: code%d diagnostic%q", code, output.String())
			}
		})
	}
}

func TestFinishRunPreservesPureWrappedInterruption(t *testing.T) {
	for _, code := range []int{0, 129} {
		m := fixtureModel(t)
		m.exitCode = code
		var output bytes.Buffer
		err := fmt.Errorf("wrapped interruption: %w", tea.ErrInterrupted)
		if got := finishRun(Options{Error: &output}, m, m.tracker, err, shutdownEvidence{}); got != code || output.Len() == 0 {
			t.Fatalf("pure interruption behavior changed: code%d want%d diagnostic%q", got, code, output.String())
		}
	}
}

func TestFinishRunUsesVerifiedTerminalHangupWithoutLosingSignalPriority(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		evidence shutdownEvidence
		want     int
	}{
		{name: "native hangup without notification", evidence: shutdownEvidence{InputHangup: true}, want: 129},
		{name: "terminate priority", evidence: shutdownEvidence{Signal: syscall.SIGTERM, InputHangup: true}, want: 143},
		{name: "interrupt priority", evidence: shutdownEvidence{Signal: os.Interrupt, InputHangup: true}, want: 130},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			m := fixtureModel(t)
			var output bytes.Buffer
			err := fmt.Errorf("%w: %w", tea.ErrProgramKilled, terminalInputError())
			if code := finishRun(Options{Input: os.Stdin, Error: &output}, m, m.tracker, err, fixture.evidence); code != fixture.want || output.Len() != 0 {
				t.Fatalf("verified terminal loss or actual signal priority lost: code%d want%d diagnostic%q", code, fixture.want, output.String())
			}
		})
	}
}

func TestExplicitForceExitKeepsUserChoiceAfterSignal(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		evidence shutdownEvidence
		err      error
		want     int
	}{
		{name: "terminate", evidence: shutdownEvidence{Signal: syscall.SIGTERM}, want: 3},
		{name: "hangup", evidence: shutdownEvidence{Signal: syscall.SIGHUP}, want: 3},
		{name: "native hangup", evidence: shutdownEvidence{InputHangup: true}, err: terminalInputError(), want: 3},
		{name: "independent fault", evidence: shutdownEvidence{Signal: syscall.SIGHUP}, err: errors.New("independent failure"), want: 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			code, err := fixture.evidence.exitDecision(3, fixture.err, os.Stdin)
			if code != fixture.want {
				t.Fatalf("explicit user force choice lost: code%d want%d err%v", code, fixture.want, err)
			}
		})
	}
}

func TestFinishRunKeepsFaultsBesideNativeHangup(t *testing.T) {
	for _, fixture := range []struct {
		name string
		err  error
		poll error
	}{
		{name: "independent joined fault", err: errors.Join(terminalInputError(), errors.New("unrelated terminal failure"))},
		{name: "poll failure", err: terminalInputError(), poll: unix.EBADF},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			m := fixtureModel(t)
			var output bytes.Buffer
			evidence := shutdownEvidence{Signal: syscall.SIGHUP, InputHangup: true, ObservationError: fixture.poll}
			if code := finishRun(Options{Input: os.Stdin, Error: &output}, m, m.tracker, fixture.err, evidence); code != 1 || output.Len() == 0 {
				t.Fatalf("independent failure was hidden by native hangup: code%d diagnostic%q", code, output.String())
			}
		})
	}
}

func TestTerminalPollRequiresPositiveHangupAndPreservesFaults(t *testing.T) {
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := input.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	for _, fixture := range []struct {
		name   string
		events int16
		fault  error
		hangup bool
		want   error
	}{
		{name: "no event"},
		{name: "readable", events: unix.POLLIN},
		{name: "hangup", events: unix.POLLHUP, hangup: true},
		{name: "error without hangup", events: unix.POLLERR},
		{name: "invalid descriptor wins", events: unix.POLLHUP | unix.POLLNVAL, want: unix.EBADF},
		{name: "poll failure", fault: unix.EINTR, want: unix.EINTR},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			calls := 0
			hangup, pollErr := pollInputHangup(input, func(fds []unix.PollFd, timeout int) (int, error) {
				calls++
				if timeout != 0 || len(fds) != 1 || fds[0].Fd != int32(input.Fd()) || fds[0].Events != unix.POLLIN {
					t.Fatalf("poll did not observe the exact borrowed input without waiting: %+v timeout%d", fds, timeout)
				}
				fds[0].Revents = fixture.events
				count := 0
				if fixture.events != 0 {
					count = 1
				}
				return count, fixture.fault
			})
			if hangup != fixture.hangup || !errors.Is(pollErr, fixture.want) || calls != 1 {
				t.Fatalf("poll absence/fault became hangup: hangup%t err%v calls%d", hangup, pollErr, calls)
			}
		})
	}
}

func TestSignalWatcherRetainsDroppedAndBufferedNotifications(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		t.Run(fmt.Sprintf("already-stopped-%t", buffered), func(t *testing.T) {
			signals := make(chan os.Signal, 1)
			done := make(chan struct{})
			joined := make(chan os.Signal, 1)
			signals <- syscall.SIGHUP
			if buffered {
				close(done)
			}
			watchSignals(signals, done, joined, func(tea.Msg) {
				// A terminated program's Send drops this message. No model
				// update is needed to preserve the actual notification.
				if !buffered {
					close(done)
				}
			})
			if actual := <-joined; actual != syscall.SIGHUP {
				t.Fatalf("actual hangup was lost without a rendered message: %v", actual)
			}
		})
	}
}

func TestRepeatedSignalsRetainLatestForceExitCode(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		values []os.Signal
		want   int
	}{
		{name: "hangup then terminate", values: []os.Signal{syscall.SIGHUP, syscall.SIGTERM}, want: 143},
		{name: "terminate then hangup", values: []os.Signal{syscall.SIGTERM, syscall.SIGHUP}, want: 129},
		{name: "interrupt", values: []os.Signal{os.Interrupt}, want: 130},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			signals := make(chan os.Signal, len(fixture.values))
			for _, value := range fixture.values {
				signals <- value
			}
			done := make(chan struct{})
			close(done)
			joined := make(chan os.Signal, 1)
			watchSignals(signals, done, joined, func(tea.Msg) {})
			m := fixtureModel(t)
			var output bytes.Buffer
			if code := finishRun(Options{Error: &output}, m, m.tracker, nil, shutdownEvidence{Signal: <-joined}); code != fixture.want || output.Len() != 0 {
				t.Fatalf("repeat signal precedence changed: code%d want%d diagnostic%q", code, fixture.want, output.String())
			}
		})
	}
}
