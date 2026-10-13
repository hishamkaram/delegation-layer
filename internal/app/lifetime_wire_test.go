package app

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type lifetimeWireScope struct {
	mu           sync.Mutex
	configured   int
	observed     int
	terminated   int
	empty        int
	closed       int
	terminateErr error
}

func (s *lifetimeWireScope) Configure(*exec.Cmd) error {
	s.mu.Lock()
	s.configured++
	s.mu.Unlock()
	return nil
}

func (s *lifetimeWireScope) ObserveStarted(pid int) (lifetime.LaunchRef, error) {
	s.mu.Lock()
	s.observed++
	s.mu.Unlock()
	return lifetime.NewLaunchRef(pid, "test-start", pid)
}

func (s *lifetimeWireScope) Terminate(context.Context, []lifetime.LaunchRef) error {
	s.mu.Lock()
	s.terminated++
	err := s.terminateErr
	s.mu.Unlock()
	return err
}

func (s *lifetimeWireScope) EmptyContext(context.Context, []lifetime.LaunchRef) (bool, error) {
	s.mu.Lock()
	s.empty++
	s.mu.Unlock()
	return true, nil
}

func (s *lifetimeWireScope) Release() error {
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
	return nil
}

type lifetimeWireExpectation struct {
	kind     string
	sequence uint64
	pid      int
	accepted bool
	code     string
}

type lifetimeWireNoopLaunchGate struct{}

func (lifetimeWireNoopLaunchGate) started(error) error { return nil }
func (lifetimeWireNoopLaunchGate) release() error      { return nil }
func (lifetimeWireNoopLaunchGate) abort() error        { return nil }

func lifetimeWireNoopLaunchGateFactory(*exec.Cmd) (lifetimeCommandLaunchGate, error) {
	return lifetimeWireNoopLaunchGate{}, nil
}

func startLifetimeWireServer(guardianConn net.Conn, taskID string, expected []lifetimeWireExpectation) <-chan error {
	serverErr := make(chan error, 1)
	go func() {
		codec, err := newLifetimeCodec(guardianConn)
		if err != nil {
			serverErr <- errors.Join(err, guardianConn.Close())
			return
		}
		defer func() { serverErr <- errors.Join(err, codec.Close()) }()
		for _, want := range expected {
			message, receiveErr := codec.receive()
			if receiveErr != nil {
				err = errors.Join(err, receiveErr)
				return
			}
			if message.Type != want.kind || message.Sequence != want.sequence || message.PID != want.pid || message.TaskID != taskID {
				err = errors.Join(err, task.ErrEvidenceFault)
				return
			}
			if replyErr := codec.reply(message, want.accepted, want.code); replyErr != nil {
				err = errors.Join(err, replyErr)
				return
			}
		}
	}()
	return serverErr
}

func TestLifetimeWorkerClientProtocolOrder(t *testing.T) {
	workerConn, guardianConn := net.Pipe()
	taskID := strings.Repeat("a", 32)
	serverErr := startLifetimeWireServer(guardianConn, taskID, []lifetimeWireExpectation{
		{kind: lifetimeMessageHello, accepted: true},
		{kind: lifetimeMessageBegin, sequence: 1, accepted: true},
		{kind: lifetimeMessageStarted, sequence: 1, pid: 123, accepted: true},
		{kind: lifetimeMessageFinish, sequence: 1, accepted: true},
		{kind: lifetimeMessageClose, accepted: true},
	})

	scope := &lifetimeWireScope{}
	client, err := newLifetimeWorkerClient(workerConn, scope, taskID, time.Now().Add(time.Minute), lifetimeWireNoopLaunchGateFactory)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("provider")
	if err = client.StartCommand(cmd, func() error {
		cmd.Process = &os.Process{Pid: 123}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = client.FinishCommand(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	if err = client.CloseTask(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
	if scope.configured != 1 || scope.closed != 1 {
		t.Fatalf("scope calls configured=%d closed=%d", scope.configured, scope.closed)
	}
	if scope.observed != 0 || scope.terminated != 0 || scope.empty != 0 {
		t.Fatalf("unexpected cleanup calls observed=%d terminated=%d empty=%d", scope.observed, scope.terminated, scope.empty)
	}
}

func TestLifetimeWorkerClientDefersStartedReplyFailureUntilAfterWait(t *testing.T) {
	workerConn, guardianConn := net.Pipe()
	taskID := strings.Repeat("d", 32)
	serverErr := startLifetimeWireServer(guardianConn, taskID, []lifetimeWireExpectation{
		{kind: lifetimeMessageHello, accepted: true},
		{kind: lifetimeMessageBegin, sequence: 1, accepted: true},
		{kind: lifetimeMessageStarted, sequence: 1, pid: 321, code: lifetimeGuardianCodeStarted},
	})

	scope := &lifetimeWireScope{terminateErr: lifetime.ErrUnverifiableLaunch}
	client, err := newLifetimeWorkerClient(workerConn, scope, taskID, time.Now().Add(time.Minute), lifetimeWireNoopLaunchGateFactory)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("provider")
	if err = client.StartCommand(cmd, func() error {
		cmd.Process = &os.Process{Pid: 321}
		return nil
	}); err != nil {
		t.Fatalf("a started process must remain owned by execution: %v", err)
	}
	if err = client.FinishCommand(t.Context(), cmd); err == nil || !strings.Contains(err.Error(), lifetimeGuardianCodeStarted) {
		t.Fatalf("finish error = %v, want deferred guardian refusal", err)
	} else if errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("already-empty local cleanup retained a stale termination error: %v", err)
	}
	if err = client.CloseTask(t.Context()); err == nil || !strings.Contains(err.Error(), lifetimeGuardianCodeStarted) {
		t.Fatalf("close error = %v, want retained guardian refusal", err)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
	if scope.configured != 1 || scope.closed != 1 {
		t.Fatalf("scope calls configured=%d closed=%d", scope.configured, scope.closed)
	}
	if scope.observed != 1 || scope.terminated != 1 || scope.empty != 1 {
		t.Fatalf("cleanup calls observed=%d terminated=%d empty=%d, want 1/1/1", scope.observed, scope.terminated, scope.empty)
	}
}

func TestLifetimeWorkerClientTreatsAcceptedTerminationAsTerminal(t *testing.T) {
	workerConn, guardianConn := net.Pipe()
	taskID := strings.Repeat("e", 32)
	serverErr := startLifetimeWireServer(guardianConn, taskID, []lifetimeWireExpectation{
		{kind: lifetimeMessageHello, accepted: true},
		{kind: lifetimeMessageBegin, sequence: 1, accepted: true},
		{kind: lifetimeMessageStarted, sequence: 1, pid: 654, accepted: true},
		{kind: lifetimeMessageStop, accepted: true},
	})

	scope := &lifetimeWireScope{}
	client, err := newLifetimeWorkerClient(workerConn, scope, taskID, time.Now().Add(time.Minute), lifetimeWireNoopLaunchGateFactory)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("provider")
	if err = client.StartCommand(cmd, func() error {
		cmd.Process = &os.Process{Pid: 654}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = client.Terminate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = client.FinishCommand(t.Context(), cmd); err != nil {
		t.Fatalf("finish after accepted termination: %v", err)
	}
	if err = client.CloseTask(t.Context()); err != nil {
		t.Fatalf("close after accepted termination: %v", err)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestLifetimeWorkerClientTerminationDoesNotWaitForBlockedStart(t *testing.T) {
	workerConn, guardianConn := net.Pipe()
	taskID := strings.Repeat("7", 32)
	serverErr := startLifetimeWireServer(guardianConn, taskID, []lifetimeWireExpectation{
		{kind: lifetimeMessageHello, accepted: true},
		{kind: lifetimeMessageBegin, sequence: 1, accepted: true},
		{kind: lifetimeMessageStop, accepted: true},
	})

	client, err := newLifetimeWorkerClient(
		workerConn, &lifetimeWireScope{}, taskID, time.Now().Add(time.Minute),
		lifetimeWireNoopLaunchGateFactory,
	)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("provider")
	startEntered := make(chan struct{})
	startRelease := make(chan struct{})
	startDone := make(chan error, 1)
	go func() {
		startDone <- client.StartCommand(cmd, func() error {
			close(startEntered)
			<-startRelease
			cmd.Process = &os.Process{Pid: 777}
			return nil
		})
	}()
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		t.Fatal("StartCommand did not enter the native start callback")
	}

	terminateDone := make(chan error, 1)
	go func() { terminateDone <- client.Terminate(t.Context()) }()
	select {
	case err = <-terminateDone:
		if err != nil {
			t.Fatalf("terminate while native start was blocked: %v", err)
		}
	case <-time.After(time.Second):
		close(startRelease)
		<-startDone
		<-terminateDone
		t.Fatal("termination waited for the blocked native start")
	}

	close(startRelease)
	if err = <-startDone; err != nil {
		t.Fatalf("started process was not retained for the execution-owned wait: %v", err)
	}
	if err = client.FinishCommand(t.Context(), cmd); err != nil {
		t.Fatalf("finish after termination during start: %v", err)
	}
	if err = client.CloseTask(t.Context()); err != nil {
		t.Fatalf("close after termination during start: %v", err)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestLifetimeWorkerClientTerminationUsesIndependentControlDeadline(t *testing.T) {
	workerConn, guardianConn := net.Pipe()
	taskID := strings.Repeat("e", 32)
	serverErr := startLifetimeWireServer(guardianConn, taskID, []lifetimeWireExpectation{
		{kind: lifetimeMessageHello, accepted: true},
		{kind: lifetimeMessageStop, accepted: true},
	})

	scope := &lifetimeWireScope{}
	client, err := newLifetimeWorkerClient(workerConn, scope, taskID, time.Now().Add(time.Minute), lifetimeWireNoopLaunchGateFactory)
	if err != nil {
		t.Fatal(err)
	}
	client.deadline = time.Now().Add(-time.Second)
	if err = client.Terminate(t.Context()); err != nil {
		t.Fatalf("terminate after task deadline: %v", err)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestLifetimeWorkerClientFinishUsesIndependentControlDeadline(t *testing.T) {
	workerConn, guardianConn := net.Pipe()
	taskID := strings.Repeat("8", 32)
	serverErr := startLifetimeWireServer(guardianConn, taskID, []lifetimeWireExpectation{
		{kind: lifetimeMessageHello, accepted: true},
		{kind: lifetimeMessageBegin, sequence: 1, accepted: true},
		{kind: lifetimeMessageStarted, sequence: 1, pid: 123, accepted: true},
		{kind: lifetimeMessageFinish, sequence: 1, accepted: true},
	})

	scope := &lifetimeWireScope{}
	client, err := newLifetimeWorkerClient(workerConn, scope, taskID, time.Now().Add(time.Minute), lifetimeWireNoopLaunchGateFactory)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("provider")
	if err = client.StartCommand(cmd, func() error {
		cmd.Process = &os.Process{Pid: 123}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	client.deadline = time.Now().Add(-time.Second)
	if err = client.FinishCommand(t.Context(), cmd); err != nil {
		t.Fatalf("finish after task deadline: %v", err)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestLifetimeWorkerClientCloseUsesIndependentControlDeadline(t *testing.T) {
	workerConn, guardianConn := net.Pipe()
	taskID := strings.Repeat("9", 32)
	serverErr := startLifetimeWireServer(guardianConn, taskID, []lifetimeWireExpectation{
		{kind: lifetimeMessageHello, accepted: true},
		{kind: lifetimeMessageClose, accepted: true},
	})

	scope := &lifetimeWireScope{}
	client, err := newLifetimeWorkerClient(workerConn, scope, taskID, time.Now().Add(time.Minute), lifetimeWireNoopLaunchGateFactory)
	if err != nil {
		t.Fatal(err)
	}
	client.deadline = time.Now().Add(-time.Second)
	if err = client.CloseTask(t.Context()); err != nil {
		t.Fatalf("close after task deadline: %v", err)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestLifetimeWorkerClientBindsHelloToImmutableDeadline(t *testing.T) {
	workerConn, guardianConn := net.Pipe()
	deadlineConn := &lifetimeGuardianDeadlineConn{Conn: workerConn, deadline: make(chan time.Time, 1)}
	taskID := strings.Repeat("f", 32)
	serverErr := startLifetimeWireServer(guardianConn, taskID, []lifetimeWireExpectation{
		{kind: lifetimeMessageHello, accepted: true},
	})
	deadline := time.Now().Add(2 * time.Second)
	scope := &lifetimeWireScope{}
	client, err := newLifetimeWorkerClient(deadlineConn, scope, taskID, deadline, lifetimeWireNoopLaunchGateFactory)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-deadlineConn.deadline; !got.Equal(deadline) {
		t.Fatalf("hello deadline = %v, want immutable deadline %v", got, deadline)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
	if scope.closed != 1 {
		t.Fatalf("scope release calls = %d, want 1", scope.closed)
	}
}

func TestLifetimeWorkerClientBoundsHelloByControlTimeout(t *testing.T) {
	workerConn, guardianConn := net.Pipe()
	deadlineConn := &lifetimeGuardianDeadlineConn{Conn: workerConn, deadline: make(chan time.Time, 1)}
	taskID := strings.Repeat("0", 32)
	serverErr := startLifetimeWireServer(guardianConn, taskID, []lifetimeWireExpectation{
		{kind: lifetimeMessageHello, accepted: true},
	})
	taskDeadline := time.Now().Add(2 * time.Minute)
	started := time.Now()
	client, err := newLifetimeWorkerClient(
		deadlineConn, &lifetimeWireScope{}, taskID, taskDeadline,
		lifetimeWireNoopLaunchGateFactory,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := <-deadlineConn.deadline
	if got.Before(started.Add(lifetimeControlTimeout-time.Second)) ||
		got.After(time.Now().Add(lifetimeControlTimeout)) || !got.Before(taskDeadline) {
		t.Fatalf("hello deadline = %v, want bounded by %v and before task deadline %v", got, lifetimeControlTimeout, taskDeadline)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestLifetimeCodecCallHonorsContext(t *testing.T) {
	workerConn, guardianConn := net.Pipe()
	cleanupLifetimeTestResource(t, guardianConn)
	codec, err := newLifetimeCodec(workerConn)
	if err != nil {
		t.Fatal(err)
	}
	cleanupLifetimeTestResource(t, codec)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err = codec.callContext(ctx, lifetimeMessage{Version: lifetimeProtocolVersion, Type: lifetimeMessageHello, TaskID: strings.Repeat("b", 32)})
	if err == nil {
		t.Fatal("silent guardian did not bound the control call")
	}
}

func TestLifetimeCodecRejectsOversizedFrame(t *testing.T) {
	workerConn, guardianConn := net.Pipe()
	cleanupLifetimeTestResource(t, workerConn)
	cleanupLifetimeTestResource(t, guardianConn)
	codec, err := newLifetimeCodec(guardianConn)
	if err != nil {
		t.Fatal(err)
	}
	cleanupLifetimeTestResource(t, codec)
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := workerConn.Write(append(make([]byte, task.MaxControlRecordSize), '\n'))
		writeDone <- errors.Join(writeErr, workerConn.Close())
	}()
	if _, err = codec.receive(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("oversized frame error = %v, want evidence fault", err)
	}
	if err = <-writeDone; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}

func TestLifetimeReplyRequiresCodeOnlyForRefusal(t *testing.T) {
	request := lifetimeMessage{Version: lifetimeProtocolVersion, Type: lifetimeMessageHello, TaskID: strings.Repeat("c", 32)}
	for _, reply := range []lifetimeReply{
		{Version: lifetimeProtocolVersion, Type: lifetimeMessageHello, Accepted: true, Code: "unexpected"},
		{Version: lifetimeProtocolVersion, Type: lifetimeMessageHello, Accepted: false},
	} {
		if err := validateLifetimeReply(reply, request); err == nil {
			t.Fatalf("invalid reply accepted: %+v", reply)
		}
	}
}
