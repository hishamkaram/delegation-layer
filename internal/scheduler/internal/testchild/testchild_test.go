package testchild

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	childFlag = "--scheduler-testchild"
	// modeEcho waits for its barrier, then prints done.
	modeEcho = "echo"
	// modeRelay starts an echo grandchild through Start and reports it.
	modeRelay = "relay"
	// modeExit exits with code 7 without reading its barrier.
	modeExit    = "exit"
	exitCode    = 7
	ownDeadline = 30 * time.Second
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == childFlag {
		os.Exit(runChild(os.Args[2]))
	}
	os.Exit(m.Run())
}

func runChild(mode string) int {
	ctx, cancel := context.WithTimeout(context.Background(), ownDeadline)
	defer cancel()
	var err error
	switch mode {
	case modeEcho:
		if err = AwaitRelease(ctx, "ready"); err == nil {
			_, err = fmt.Fprintln(os.Stdout, "done")
		}
	case modeRelay:
		err = relay()
	case modeExit:
		return exitCode
	default:
		err = fmt.Errorf("unknown child mode %q", mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return 0
}

func relay() error {
	grandchild, err := Start(childFlag, modeEcho)
	if err != nil {
		_, printErr := fmt.Fprintf(os.Stdout, "start failed: %v\n", err)
		return printErr
	}
	ready, readErr := grandchild.ReadLine()
	code, waitErr := grandchild.Wait()
	if joinErr := errors.Join(readErr, waitErr); joinErr != nil || ready != "ready" || code != 0 {
		return fmt.Errorf("grandchild said %q, exited %d: %w", ready, code, joinErr)
	}
	_, err = fmt.Fprintln(os.Stdout, "relayed")
	return err
}

func requireClosed(t *testing.T, files ...*os.File) {
	t.Helper()
	for _, file := range files {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Errorf("pipe end %s still open: %v", file.Name(), err)
		}
	}
}

// recordingPipes opens real pipes, failing the call numbered fail.
type recordingPipes struct {
	fail  int
	calls int
	files []*os.File
}

func (p *recordingPipes) open() (*os.File, *os.File, error) {
	p.calls++
	if p.calls == p.fail {
		return nil, nil, errors.New("injected pipe failure")
	}
	reader, writer, err := os.Pipe()
	if err == nil {
		p.files = append(p.files, reader, writer)
	}
	return reader, writer, err
}

func TestSetupFaultClosesTheOpenedPipe(t *testing.T) {
	pipes := &recordingPipes{fail: 2}
	executable, err := Executable()
	if err != nil {
		t.Fatal(err)
	}
	if child, err := start(exec.Command(executable, childFlag, modeEcho), pipes.open); err == nil || child != nil {
		t.Fatalf("start with a failing stdout pipe = %v, %v", child, err)
	}
	if len(pipes.files) != 2 {
		t.Fatalf("opened %d pipe ends, want the stdin pair", len(pipes.files))
	}
	requireClosed(t, pipes.files...)
}

func TestStartFaultClosesEveryPipeEnd(t *testing.T) {
	pipes := &recordingPipes{}
	missing := filepath.Join(t.TempDir(), "missing-binary")
	if child, err := start(exec.Command(missing), pipes.open); err == nil || child != nil {
		t.Fatalf("start of a missing binary = %v, %v", child, err)
	}
	if len(pipes.files) != 4 {
		t.Fatalf("opened %d pipe ends, want both pairs", len(pipes.files))
	}
	requireClosed(t, pipes.files...)
}

func TestHeldBarrierEndsAtTheChildDeadline(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeAll(reader, writer); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- awaitEOF(ctx, reader) }()
	bound := time.NewTimer(ownDeadline)
	defer bound.Stop()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("held barrier err=%v, want the deadline", err)
		}
	case <-bound.C:
		t.Fatal("barrier ignored its deadline while stdin stayed open")
	}
}

func TestWaitJoinsOnceAndRepeatsItsResult(t *testing.T) {
	child, err := Start(childFlag, modeExit)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if code, err := child.Wait(); err != nil || code != exitCode {
			t.Fatalf("wait = %d, %v; want exit %d without error", code, err, exitCode)
		}
	}
	if err := child.CloseInput(); err != nil {
		t.Fatalf("close input after wait: %v", err)
	}
}

func TestEarlyCallerExitStillReleasesAndJoins(t *testing.T) {
	var child *Child
	t.Run("caller returns before releasing", func(t *testing.T) {
		child = Launch(t, childFlag, modeEcho)
		if line := child.Line(t); line != "ready" {
			t.Fatalf("child said %q", line)
		}
	})
	if child.cmd.ProcessState == nil {
		t.Fatal("cleanup did not join the child")
	}
	if code, err := child.Wait(); err != nil || code != 0 {
		t.Fatalf("joined child = %d, %v", code, err)
	}
}

func TestRelativeInvocationReExecsAGrandchild(t *testing.T) {
	executable, err := Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, childFlag, modeRelay)
	cmd.Args[0] = "./" + filepath.Base(executable)
	cmd.Dir = filepath.Dir(executable)
	cmd.Stderr = os.Stderr
	child, err := start(cmd, os.Pipe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := child.Wait(); err != nil {
			t.Error(err)
		}
	})
	child.Release(t)
	if line := child.Line(t); line != "relayed" {
		t.Fatalf("relative launch said %q, want relayed", line)
	}
	if code := child.Finish(t); code != 0 {
		t.Fatalf("relay exited %d", code)
	}
}
