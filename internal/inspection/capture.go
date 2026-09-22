package inspection

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/hishamkaram/delegation-layer/internal/provider"
)

var errNativeInspection = errors.New("native inspection unavailable")

// nativeHooks replaces owned operations in deterministic tests. Production
// callers supply no hooks. Neither callbacks nor diagnostics reach the journal.
type nativeHooks struct {
	start func(*exec.Cmd) error
	wait  func(*exec.Cmd) error
}

// nativeScope is the small lifetime boundary needed by native inspection. The
// production implementation is execution.PreflightScope; keeping the
// interface here prevents inspection from owning task or process lifetimes.
type nativeScope interface {
	Context() context.Context
	Start(func() error) error
}

// runNative is private to the supervised inspection lifetime. The scope owns
// the final start gate. Cancellation refuses a new start, but never abandons a
// started command or its pipes. On success the caller owns the returned
// sensitive buffer and must clear it after projection.
func runNative(scope nativeScope, definition provider.InspectionDefinition, hooks nativeHooks) ([]byte, error) {
	if scope == nil {
		return nil, errNativeInspection
	}
	ctx := scope.Context()
	definition, _, err := definition.Snapshot()
	if err != nil || ctx == nil {
		return nil, errNativeInspection
	}
	if _, finite := ctx.Deadline(); !finite || ctx.Err() != nil {
		return nil, errNativeInspection
	}
	stdout, err := newNativeStream(definition.OutputLimit, true)
	if err != nil {
		return nil, errNativeInspection
	}
	stderr, err := newNativeStream(definition.OutputLimit, false)
	if err != nil {
		stdout.discard()
		return nil, errNativeInspection
	}
	// A nil Stdin makes the child receive finite EOF. The command is bound to
	// the executable observed during admission before the final Start gate.
	cmd, verified, err := newVerifiedCommand(definition.Executable, definition.ExecutableSHA256, definition.Directory, definition.Environment, definition.Arguments...)
	if err != nil {
		stdout.discard()
		stderr.discard()
		return nil, errNativeInspection
	}
	cmd.Dir, cmd.Env = definition.Directory, definition.Environment
	cmd.Stdout, cmd.Stderr = stdout.writer, stderr.writer
	go stdout.drain()
	go stderr.drain()
	startErr := scope.Start(func() error { return hooks.startCommand(cmd) })
	closeErr := errors.Join(stdout.writer.Close(), stderr.writer.Close())
	if startErr == nil {
		closeErr = errors.Join(closeErr, verified.ReleaseDescriptors())
	}
	var waitErr error
	if startErr == nil {
		waitErr = hooks.waitCommand(cmd)
	}
	stdoutErr, stderrErr := <-stdout.done, <-stderr.done
	closeErr = errors.Join(closeErr, verified.Close())
	if errors.Join(startErr, closeErr, waitErr, stdoutErr, stderrErr, ctx.Err()) != nil {
		clear(stdout.buffer.bytes)
		return nil, errNativeInspection
	}
	return stdout.buffer.bytes, nil
}

func (h nativeHooks) startCommand(cmd *exec.Cmd) error {
	if h.start != nil {
		return h.start(cmd)
	}
	return cmd.Start()
}

func (h nativeHooks) waitCommand(cmd *exec.Cmd) error {
	if h.wait != nil {
		return h.wait(cmd)
	}
	if err := cmd.Wait(); err != nil {
		return errNativeInspection
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		return errNativeInspection
	}
	return nil
}

type nativeStream struct {
	reader *os.File
	writer *os.File
	buffer boundedNativeBuffer
	done   chan error
}

func newNativeStream(limit int64, retain bool) (*nativeStream, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errNativeInspection
	}
	buffer := boundedNativeBuffer{remaining: limit, retain: retain}
	if retain {
		buffer.bytes = make([]byte, 0, limit)
	}
	return &nativeStream{reader: reader, writer: writer, buffer: buffer, done: make(chan error, 1)}, nil
}

func (s *nativeStream) discard() {
	// No command was started; no sensitive diagnostics or close errors escape.
	if err := errors.Join(s.reader.Close(), s.writer.Close()); err != nil {
		return
	}
}

func (s *nativeStream) drain() {
	buffer := make([]byte, 32*1024)
	// Hide File.WriteTo so io.CopyBuffer uses this explicitly owned buffer.
	_, copyErr := io.CopyBuffer(&s.buffer, struct{ io.Reader }{s.reader}, buffer)
	clear(buffer)
	closeErr := s.reader.Close()
	if copyErr != nil || closeErr != nil || s.buffer.overflow {
		s.done <- errNativeInspection
		return
	}
	s.done <- nil
}

// Once the cap is reached, Write still acknowledges every byte so capture
// drains to EOF without forcing a child failure or retaining excess secrets.
type boundedNativeBuffer struct {
	remaining int64
	retain    bool
	overflow  bool
	bytes     []byte
}

func (b *boundedNativeBuffer) Write(data []byte) (int, error) {
	accepted := min(int64(len(data)), b.remaining)
	if b.retain {
		b.bytes = append(b.bytes, data[:accepted]...)
	}
	b.remaining -= accepted
	if accepted != int64(len(data)) {
		b.overflow = true
	}
	return len(data), nil
}
