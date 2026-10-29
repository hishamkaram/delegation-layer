// Package testchild runs the current test binary again as one finite helper
// child for the scheduler's process tests. It owns every pipe end from the
// moment it is created, waits for each started child exactly once, and never
// signals a process. Each consumer keeps its own helper modes and actions.
package testchild

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Child is one started helper process. Its stdin is the start barrier: the
// child proceeds once stdin reaches EOF.
type Child struct {
	cmd    *exec.Cmd
	input  *os.File
	output *os.File
	lines  *bufio.Reader

	mu       sync.Mutex
	released bool
	waited   bool
	code     int
	waitErr  error
}

// Executable returns the absolute path of the running test binary. It never
// uses os.Args[0], which may be relative to a launch directory the child does
// not share or a bare name that exec would resolve through PATH.
func Executable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating the test binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolving the test binary: %w", err)
	}
	return resolved, nil
}

// Start runs the test binary with args as a child.
func Start(args ...string) (*Child, error) {
	executable, err := Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(executable, args...)
	cmd.Stderr = os.Stderr
	return start(cmd, os.Pipe)
}

// start wires two pipes into cmd and starts it. On any fault it closes every
// pipe end it opened. After a successful start the child's ends are closed
// here, so EOF on either side means the other side is gone.
func start(cmd *exec.Cmd, openPipe func() (*os.File, *os.File, error)) (*Child, error) {
	childInput, input, err := openPipe()
	if err != nil {
		return nil, fmt.Errorf("creating the child's stdin: %w", err)
	}
	output, childOutput, err := openPipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("creating the child's stdout: %w", err), closeAll(childInput, input))
	}
	cmd.Stdin, cmd.Stdout = childInput, childOutput
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("starting the child: %w", err), closeAll(childInput, input, output, childOutput))
	}
	if err := closeAll(childInput, childOutput); err != nil {
		child := &Child{cmd: cmd, input: input, output: output, lines: bufio.NewReader(output)}
		_, finishErr := child.Wait()
		return nil, errors.Join(err, finishErr)
	}
	return &Child{cmd: cmd, input: input, output: output, lines: bufio.NewReader(output)}, nil
}

// closeAll closes every file and reports each failure.
func closeAll(files ...*os.File) error {
	var faults []error
	for _, file := range files {
		if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			faults = append(faults, err)
		}
	}
	return errors.Join(faults...)
}

// ReadLine returns the child's next stdout line without its newline.
func (c *Child) ReadLine() (string, error) {
	text, err := c.lines.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading the child's output: %w", err)
	}
	return strings.TrimSuffix(text, "\n"), nil
}

// CloseInput closes the child's stdin, releasing its start barrier. It is
// idempotent.
func (c *Child) CloseInput() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeInputLocked()
}

func (c *Child) closeInputLocked() error {
	if c.released {
		return nil
	}
	c.released = true
	return closeAll(c.input)
}

// Wait releases the barrier, drains the remaining output, waits for the child
// once, and returns its exit code. Later calls return the same result. A
// nonzero exit is a code, not an error.
func (c *Child) Wait() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waited {
		return c.code, c.waitErr
	}
	c.waited = true
	releaseErr := c.closeInputLocked()
	_, drainErr := io.Copy(io.Discard, c.lines)
	waitErr := c.cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		waitErr = nil
	}
	c.code = c.cmd.ProcessState.ExitCode()
	c.waitErr = errors.Join(releaseErr, drainErr, waitErr, closeAll(c.output))
	return c.code, c.waitErr
}

// Launch starts a child for a test and registers its Wait as cleanup, so a
// test that fails or returns early still releases and joins the child.
func Launch(t testing.TB, args ...string) *Child {
	t.Helper()
	child, err := Start(args...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := child.Wait(); err != nil {
			t.Errorf("joining helper child: %v", err)
		}
	})
	return child
}

// Line returns the child's next stdout line or fails the test.
func (c *Child) Line(t testing.TB) string {
	t.Helper()
	line, err := c.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// Release closes the child's start barrier or fails the test.
func (c *Child) Release(t testing.TB) {
	t.Helper()
	if err := c.CloseInput(); err != nil {
		t.Fatal(err)
	}
}

// Finish joins the child and returns its exit code, or fails the test.
func (c *Child) Finish(t testing.TB) int {
	t.Helper()
	code, err := c.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// AwaitRelease runs in the child: it reports ready on stdout, then waits until
// stdin reaches EOF or ctx ends. On ctx end the reading goroutine is left to
// the exiting child process.
func AwaitRelease(ctx context.Context, ready string) error {
	if _, err := fmt.Fprintln(os.Stdout, ready); err != nil {
		return err
	}
	return awaitEOF(ctx, os.Stdin)
}

func awaitEOF(ctx context.Context, input io.Reader) error {
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, input)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("waiting for the start barrier: %w", ctx.Err())
	}
}
