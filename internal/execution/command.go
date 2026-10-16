package execution

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
)

// CommandScope is the narrow lifetime boundary required by one supervised
// command. The scope owns admission and descendant termination; this package
// owns the command's pipes, capture workers, and exactly one Wait.
type CommandScope interface {
	Context() context.Context
	StartCommand(*exec.Cmd, func() error) error
	FinishCommand(*exec.Cmd) error
}

// CommandHooks replace the command operations used by deterministic tests and
// verified callers. Started runs after the parent pipe writers are closed and
// before the command is waited, which lets callers release descriptors that
// were needed only during Start.
type CommandHooks struct {
	Start   func(*exec.Cmd) error
	Wait    func(*exec.Cmd) error
	Started func() error
}

var errInvalidCommandScope = errors.New("invalid command scope")

// RunScopedCommand runs one command with manually-owned stdout and stderr
// pipes. The command's existing Stdout and Stderr writers are the capture
// destinations; nil destinations are treated as io.Discard. The caller owns
// Stdin, and a nil Stdin retains exec.Cmd's finite EOF behavior.
//
// Start, parent-pipe closure, Started, Wait, FinishCommand, and stream joins
// are deliberately ordered. FinishCommand must run before waiting for stream
// EOF because descendants may retain inherited pipe writers until the guardian
// terminates them. Context is used only to reject a new command and report an
// expired scope; it never stops a process.
func RunScopedCommand(scope CommandScope, cmd *exec.Cmd, hooks CommandHooks) error {
	ctx, err := validateCommandScope(scope, cmd)
	if err != nil {
		return err
	}

	originalStdout := cmd.Stdout
	originalStderr := cmd.Stderr
	stdout, err := newCommandStream(commandWriter(originalStdout))
	if err != nil {
		return err
	}
	stderr, err := newCommandStream(commandWriter(originalStderr))
	if err != nil {
		return joinCommandErrors(err, stdout.close(), stdout.join())
	}
	cmd.Stdout = stdout.writer
	cmd.Stderr = stderr.writer
	defer func() {
		cmd.Stdout = originalStdout
		cmd.Stderr = originalStderr
	}()

	start := hooks.Start
	if start == nil {
		start = func(command *exec.Cmd) error { return command.Start() }
	}
	wait := hooks.Wait
	if wait == nil {
		wait = func(command *exec.Cmd) error { return command.Wait() }
	}

	started := false
	startErr := scope.StartCommand(cmd, func() error {
		commandErr := start(cmd)
		started = commandErr == nil
		return commandErr
	})
	parentErr := errors.Join(stdout.writer.Close(), stderr.writer.Close())
	var startedErr, waitErr, finishErr error
	if started {
		if hooks.Started != nil {
			startedErr = hooks.Started()
		}
		waitErr = wait(cmd)
		finishErr = scope.FinishCommand(cmd)
	}
	if startErr == nil && !started {
		startErr = errInvalidCommandScope
	}
	stdoutErr, stderrErr := stdout.join(), stderr.join()
	return joinCommandErrors(startErr, parentErr, startedErr, waitErr, finishErr, stdoutErr, stderrErr, ctx.Err())
}

func validateCommandScope(scope CommandScope, cmd *exec.Cmd) (context.Context, error) {
	if scope == nil || cmd == nil {
		return nil, errInvalidCommandScope
	}
	ctx := scope.Context()
	if ctx == nil {
		return nil, errInvalidCommandScope
	}
	deadline, finite := ctx.Deadline()
	if !finite || deadline.IsZero() || ctx.Err() != nil {
		return nil, errInvalidCommandScope
	}
	return ctx, nil
}

func commandWriter(writer io.Writer) io.Writer {
	if writer == nil {
		return io.Discard
	}
	return writer
}

type commandStream struct {
	reader *os.File
	writer *os.File
	done   chan error
}

func newCommandStream(destination io.Writer) (*commandStream, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stream := &commandStream{reader: reader, writer: writer, done: make(chan error, 1)}
	go stream.drain(destination)
	return stream, nil
}

func (s *commandStream) drain(destination io.Writer) {
	buffer := make([]byte, 32*1024)
	_, copyErr := io.CopyBuffer(struct{ io.Writer }{destination}, struct{ io.Reader }{s.reader}, buffer)
	if copyErr != nil {
		_, drainErr := io.CopyBuffer(io.Discard, struct{ io.Reader }{s.reader}, buffer)
		copyErr = joinCommandErrors(copyErr, drainErr)
	}
	clear(buffer)
	closeErr := s.reader.Close()
	s.done <- joinCommandErrors(copyErr, closeErr)
}

func (s *commandStream) close() error {
	return errors.Join(s.writer.Close(), s.reader.Close())
}

func (s *commandStream) join() error {
	return <-s.done
}

func joinCommandErrors(errs ...error) error {
	var joined error
	for _, err := range errs {
		if err == nil {
			continue
		}
		if joined == nil {
			joined = err
			continue
		}
		joined = errors.Join(joined, err)
	}
	return joined
}
