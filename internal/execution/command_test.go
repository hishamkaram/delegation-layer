package execution

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"slices"
	"sync"
	"testing"
	"time"
)

type commandTestScope struct {
	ctx    context.Context
	start  func(*exec.Cmd, func() error) error
	finish func(*exec.Cmd) error
}

func (s commandTestScope) Context() context.Context { return s.ctx }

func (s commandTestScope) StartCommand(cmd *exec.Cmd, start func() error) error {
	if s.start != nil {
		return s.start(cmd, start)
	}
	return start()
}

func (s commandTestScope) FinishCommand(cmd *exec.Cmd) error {
	if s.finish != nil {
		return s.finish(cmd)
	}
	return nil
}

func commandTestContext(t *testing.T) context.Context {
	t.Helper()
	return func() context.Context {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
		t.Cleanup(cancel)
		return ctx
	}()
}

type commandEventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *commandEventLog) add(event string) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *commandEventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

type blockingCommandWriter struct {
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
	log         *commandEventLog
}

func newBlockingCommandWriter(log *commandEventLog) *blockingCommandWriter {
	return &blockingCommandWriter{
		started: make(chan struct{}),
		release: make(chan struct{}),
		log:     log,
	}
}

func (w *blockingCommandWriter) Write(data []byte) (int, error) {
	w.startOnce.Do(func() {
		w.log.add("capture-start")
		close(w.started)
	})
	<-w.release
	w.log.add("capture-finished")
	return len(data), nil
}

func (w *blockingCommandWriter) releaseCapture() {
	w.releaseOnce.Do(func() { close(w.release) })
}

func TestRunScopedCommandFinishesLifetimeBeforeJoiningCapture(t *testing.T) {
	log := &commandEventLog{}
	destination := newBlockingCommandWriter(log)
	ctx := commandTestContext(t)
	scope := commandTestScope{
		ctx: ctx,
		finish: func(*exec.Cmd) error {
			<-destination.started
			log.add("finish")
			destination.releaseCapture()
			return nil
		},
	}
	cmd := &exec.Cmd{Stdout: destination, Stderr: io.Discard}
	result := make(chan error, 1)
	go func() {
		result <- RunScopedCommand(scope, cmd, CommandHooks{
			Start: func(command *exec.Cmd) error {
				_, err := io.WriteString(command.Stdout, "captured")
				return err
			},
			Wait: func(*exec.Cmd) error {
				log.add("wait")
				return nil
			},
		})
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("scoped command failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		destination.releaseCapture()
		select {
		case <-result:
		case <-time.After(2 * time.Second):
			t.Fatal("scoped command did not reach FinishCommand before capture join")
		}
		t.Fatal("scoped command reached capture join before FinishCommand")
	}

	events := log.snapshot()
	waitIndex := commandEventIndex(events, "wait")
	finishIndex := commandEventIndex(events, "finish")
	captureIndex := commandEventIndex(events, "capture-finished")
	if waitIndex == len(events) || finishIndex == len(events) || captureIndex == len(events) || waitIndex > finishIndex || finishIndex > captureIndex {
		t.Fatalf("lifetime/capture order=%v", events)
	}
}

func commandEventIndex(events []string, want string) int {
	for index, event := range events {
		if event == want {
			return index
		}
	}
	return len(events)
}

func TestRunScopedCommandJoinsCaptureWhenStartFails(t *testing.T) {
	startErr := errors.New("start failed")
	var stdout, stderr bytes.Buffer
	waitCalled, finishCalled := false, false
	stdoutWriter, stderrWriter := io.Writer(&stdout), io.Writer(&stderr)
	scope := commandTestScope{
		ctx: commandTestContext(t),
		finish: func(*exec.Cmd) error {
			finishCalled = true
			return nil
		},
	}
	cmd := &exec.Cmd{Stdout: stdoutWriter, Stderr: stderrWriter}
	err := RunScopedCommand(scope, cmd, CommandHooks{
		Start: func(command *exec.Cmd) error {
			if _, writeErr := io.WriteString(command.Stdout, "out"); writeErr != nil {
				return writeErr
			}
			_, writeErr := io.WriteString(command.Stderr, "err")
			return errors.Join(startErr, writeErr)
		},
		Wait: func(*exec.Cmd) error {
			waitCalled = true
			return nil
		},
	})
	if !errors.Is(err, startErr) {
		t.Fatalf("start error=%v, want %v", err, startErr)
	}
	if waitCalled || finishCalled {
		t.Fatalf("failed start was waited or finished: wait=%t finish=%t", waitCalled, finishCalled)
	}
	if stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("capture lost on start failure: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if cmd.Stdout != stdoutWriter || cmd.Stderr != stderrWriter {
		t.Fatal("command writers were not restored after capture ownership ended")
	}
}

func TestRunScopedCommandPreservesCommandExitError(t *testing.T) {
	exitErr := &exec.ExitError{}
	var stdout bytes.Buffer
	events := []string{}
	waitCalled, finishCalled := false, false
	cmd := &exec.Cmd{Stdout: &stdout, Stderr: io.Discard}
	err := RunScopedCommand(commandTestScope{
		ctx: commandTestContext(t),
		finish: func(*exec.Cmd) error {
			events = append(events, "finish")
			finishCalled = true
			return nil
		},
	}, cmd, CommandHooks{
		Start: func(*exec.Cmd) error {
			events = append(events, "start")
			return nil
		},
		Started: func() error {
			events = append(events, "started")
			return nil
		},
		Wait: func(*exec.Cmd) error {
			events = append(events, "wait")
			waitCalled = true
			return exitErr
		},
	})
	var observed *exec.ExitError
	if !errors.As(err, &observed) || observed != exitErr {
		t.Fatalf("exit error=%v, want original *exec.ExitError", err)
	}
	if !waitCalled || !finishCalled {
		t.Fatalf("owned wait/finish missing: wait=%t finish=%t", waitCalled, finishCalled)
	}
	if want := []string{"start", "started", "wait", "finish"}; !slices.Equal(events, want) {
		t.Fatalf("command hook order=%v, want %v", events, want)
	}
}

func TestRunScopedCommandJoinsAnEnteredCommandAfterStartGateFailure(t *testing.T) {
	fault := errors.New("scope publication failed after start")
	var waited, finished bool
	scope := commandTestScope{
		ctx:    commandTestContext(t),
		start:  func(_ *exec.Cmd, start func() error) error { return errors.Join(start(), fault) },
		finish: func(*exec.Cmd) error { finished = true; return nil },
	}
	err := RunScopedCommand(scope, exec.Command("unused"), CommandHooks{
		Start: func(*exec.Cmd) error { return nil },
		Wait:  func(*exec.Cmd) error { waited = true; return nil },
	})
	if !errors.Is(err, fault) || !waited || !finished {
		t.Fatalf("entered command ownership was abandoned: err=%v waited=%t finished=%t", err, waited, finished)
	}
}

func TestRunScopedCommandRejectsScopeThatDidNotEnterStart(t *testing.T) {
	waited := false
	scope := commandTestScope{ctx: commandTestContext(t), start: func(*exec.Cmd, func() error) error { return nil }}
	err := RunScopedCommand(scope, exec.Command("unused"), CommandHooks{Wait: func(*exec.Cmd) error { waited = true; return nil }})
	if !errors.Is(err, errInvalidCommandScope) || waited {
		t.Fatalf("scope without command start was accepted: err=%v waited=%t", err, waited)
	}
}
