package probe

import (
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

var errNativeInspection = errors.New("native inspection unavailable")

// nativeHooks supplies final environment preparation and replaces owned
// operations in deterministic tests. Neither callbacks nor diagnostics reach
// the journal. Production keeps the native Start and Wait defaults.
type nativeHooks struct {
	prepareEnvironment func([]string) ([]string, error)
	start              func(*exec.Cmd) error
	wait               func(*exec.Cmd) error
}

// runNative is private to the supervised inspection lifetime. The scope owns
// the final start gate and guardian registration. Cancellation refuses a new
// start, but never abandons a started command or its pipes. On success the
// caller owns the returned sensitive buffer and must clear it after projection.
func runNative(scope execution.CommandScope, definition provider.InspectionDefinition, hooks nativeHooks) ([]byte, error) {
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
	// A nil Stdin gives the child finite EOF. Every role runs in place.
	if hooks.prepareEnvironment != nil {
		environment, prepareErr := hooks.prepareEnvironment(definition.Environment)
		if prepareErr != nil {
			return nil, errNativeInspection
		}
		definition.Environment = environment
	}
	cmd := newProviderCommand(definition.Executable, definition.Directory, definition.Environment, definition.Arguments...)
	stdout := boundedNativeBuffer{
		remaining: definition.OutputLimit,
		retain:    true,
		bytes:     make([]byte, 0, definition.OutputLimit),
	}
	stderr := boundedNativeBuffer{remaining: definition.OutputLimit}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	commandErr := execution.RunScopedCommand(scope, cmd, execution.CommandHooks{
		Start: hooks.startCommand,
		Wait:  hooks.waitCommand,
	})
	if commandErr != nil || stdout.overflow || stderr.overflow {
		clear(stdout.bytes)
		return nil, errNativeInspection
	}
	return stdout.bytes, nil
}

// newProviderCommand preserves the selected installed role and native arguments.
func newProviderCommand(executable, directory string, environment []string, args ...string) *exec.Cmd {
	cmd := exec.Command(executable, args...)
	cmd.Dir, cmd.Env = directory, environment
	return cmd
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
