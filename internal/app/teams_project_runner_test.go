package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func TestTeamsProjectRunnerHelper(t *testing.T) {
	if os.Getenv("DELEGATE_TEAMS_PROJECT_RUNNER_HELPER") != "hold" {
		return
	}
	if _, err := io.WriteString(os.Stdout, "ready\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

type teamsProjectReadyWriter struct {
	ready     chan struct{}
	probe     chan struct{}
	readyOnce sync.Once
	probeOnce sync.Once
	buffer    bytes.Buffer
}

func (writer *teamsProjectReadyWriter) Write(data []byte) (int, error) {
	writer.buffer.Write(data)
	if bytes.Contains(writer.buffer.Bytes(), []byte("ready\n")) {
		writer.readyOnce.Do(func() { close(writer.ready) })
	}
	if bytes.Contains(writer.buffer.Bytes(), []byte("probe\n")) {
		writer.probeOnce.Do(func() { close(writer.probe) })
	}
	return len(data), nil
}

type teamsProjectRunResult struct {
	done  chan struct{}
	input *os.File
	probe <-chan struct{}
	err   error // Read only after done closes; the runner goroutine is its writer.
}

func startBlockedTeamsProjectCommand(t *testing.T, ctx context.Context) (*teamsProjectRunResult, <-chan struct{}) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ready := &teamsProjectReadyWriter{ready: make(chan struct{}), probe: make(chan struct{})}
	command := exec.Command(executable, "-test.run=^TestTeamsProjectRunnerHelper$")
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(), "DELEGATE_TEAMS_PROJECT_RUNNER_HELPER=hold")
	command.Stdin, command.Stdout = reader, ready
	result := &teamsProjectRunResult{done: make(chan struct{}), input: writer, probe: ready.probe}
	go func() {
		result.err = (teamsProjectRunner{}).Run(ctx, command)
		close(result.done)
	}()
	t.Cleanup(func() {
		// EOF is the cooperative fixture release if the tested stop path fails.
		if closeErr := writer.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		<-result.done
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return result, ready.ready
}

func TestTeamsProjectRunnerConcurrentReadsKeepSeparateScopes(t *testing.T) {
	firstContext, cancelFirst := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelFirst()
	secondContext, cancelSecond := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelSecond()
	first, firstReady := startBlockedTeamsProjectCommand(t, firstContext)
	waitTeamsProjectReady(t, first, firstReady)
	second, secondReady := startBlockedTeamsProjectCommand(t, secondContext)
	waitTeamsProjectReady(t, second, secondReady)
	cancelFirst()
	select {
	case <-first.done:
	case <-time.After(15 * time.Second):
		t.Fatal("first cancellation never completed")
	}
	if _, err := io.WriteString(second.input, "probe\n"); err != nil {
		t.Fatal("sibling read was stopped:", err)
	}
	select {
	case <-second.probe:
	case <-second.done:
		t.Fatalf("stopping one read stopped its sibling: %v", second.err)
	case <-time.After(15 * time.Second):
		t.Fatal("sibling read did not answer its independent probe")
	}
	cancelSecond()
}

func waitTeamsProjectReady(t *testing.T, result *teamsProjectRunResult, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-result.done:
		t.Fatalf("native fixture failed before readiness: %v", result.err)
	case <-time.After(10 * time.Second):
		t.Fatal("native fixture never became ready")
	}
}

func TestTeamsProjectRunnerCancellationJoinsNativeCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	result, ready := startBlockedTeamsProjectCommand(t, ctx)
	select {
	case <-ready:
	case <-result.done:
		t.Fatalf("command failed before the fixture owned its blocking read: %v", result.err)
	case <-time.After(10 * time.Second):
		t.Fatal("native fixture never became ready")
	}
	cancel()
	select {
	case <-result.done:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("cancellation was not retained: %v", result.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancellation did not join the owned native command")
	}
}

func TestTeamsProjectRunnerDeadlineJoinsNativeCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, ready := startBlockedTeamsProjectCommand(t, ctx)
	select {
	case <-ready:
	case <-result.done:
		t.Fatalf("command failed before the fixture owned its blocking read: %v", result.err)
	case <-time.After(10 * time.Second):
		t.Fatal("native fixture never became ready")
	}
	select {
	case <-result.done:
		if !errors.Is(result.err, context.DeadlineExceeded) {
			t.Fatalf("deadline was not retained: %v", result.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("deadline did not join the owned native command")
	}
}
