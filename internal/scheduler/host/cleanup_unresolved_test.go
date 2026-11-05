package host

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/jonboulle/clockwork"
)

// These unit fixtures exercise the real reader-drain policy with owned pipe
// descriptors. Native child Start/Wait and frame authentication are covered by
// TestHostPermanentChildFaultDoesNotRelaunchAndKeepsControls.
func hostDrainPolicyFixture(t *testing.T, buffered bool) (*service, func()) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var once sync.Once
	finishReader := func() { once.Do(func() { close(done) }) }
	capacity := 0
	if buffered {
		capacity = 1
	}
	control := &channel{reader: reader, writer: writer, events: make(chan childEvent, capacity), done: done, stop: make(chan struct{})}
	t.Cleanup(func() {
		finishReader()
		if closeErr := control.close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	s := &service{clock: clockwork.NewFakeClock(), epoch: epochRecord{Protocol: ProtocolVersion, RootID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Epoch: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", BuildID: "native-test-build", Host: identity(self)}, status: Status{Attempt: 1, State: "ready"}, child: &childProcess{identity: identity(self), control: control}}
	t.Cleanup(func() {
		if s.retry != nil {
			s.retry.Stop()
		}
	})
	return s, finishReader
}

func TestHostPermanentChildFaultThroughBothFinalDrains(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		for _, wrong := range []bool{false, true} {
			for _, kind := range []string{"incompatible", "cleanup_unresolved"} {
				name := map[bool]string{false: "reader-running", true: "reader-complete"}[buffered] + "/" + map[bool]string{false: "bound", true: "wrong-binding"}[wrong] + "/" + kind
				t.Run(name, func(t *testing.T) { exerciseHostTerminalDrain(t, buffered, wrong, kind) })
			}
		}
	}
}

func exerciseHostTerminalDrain(t *testing.T, buffered, wrong bool, kind string) {
	t.Helper()
	s, finishReader := hostDrainPolicyFixture(t, buffered)
	event := childEvent{frame: s.binding(kind)}
	if wrong {
		event.frame.BuildID += "-wrong"
	}
	if buffered {
		s.child.control.events <- event
		finishReader()
		// Exercise the production final-queue drain after the reader has joined.
		s.drainChildFrames()
		s.exited(nil)
	} else {
		finished := make(chan struct{})
		t.Cleanup(func() { finishReader(); receive(t, finished) })
		go func() { defer close(finished); s.exited(nil) }()
		// The unbuffered send proves the still-running reader drain consumed it.
		select {
		case s.child.control.events <- event:
		case <-time.After(5 * time.Second):
			t.Fatal("reader drain did not accept terminal frame")
		}
		finishReader()
		receive(t, finished)
	}
	expected := "cleanup_unresolved"
	if kind == "incompatible" {
		expected = "incompatible_definition"
	}
	if wrong {
		expected = "child_identity_fault"
	}
	status := s.snapshot()
	if !status.Fault || status.Ready || status.LastError != expected || s.retry != nil || status.NextRetryAt != nil || s.child != nil {
		t.Fatalf("terminal frame lost across Wait/reader drain: %+v", status)
	}
}

func TestHostCleanupFrameDoesNotOverrideAcceptedStop(t *testing.T) {
	s, finishReader := hostDrainPolicyFixture(t, true)
	s.status.StopAccepted, s.status.State = true, "stopping"
	s.childEvent(context.Background(), childEvent{frame: s.binding("cleanup_unresolved")})
	finishReader()
	s.exited(nil)
	if s.status.State != "stopping" || s.retry != nil || s.status.NextRetryAt != nil || s.child != nil {
		t.Fatalf("cleanup frame overrode accepted stop: %+v", s.snapshot())
	}
}

func TestHostUnverifiedCleanupEventRetainsOrdinaryExitPolicy(t *testing.T) {
	s, finishReader := hostDrainPolicyFixture(t, true)
	s.child.control.events <- childEvent{frame: s.binding("cleanup_unresolved"), err: errors.Join(io.EOF, errors.New("incomplete frame"))}
	finishReader()
	s.exited(nil)
	if s.permanent || s.retry == nil || s.status.State != "backoff" {
		t.Fatalf("unverified frame became permanent cleanup fault: %+v", s.snapshot())
	}
}

func TestHostReportsOneCleanupFrameBeforeIncompatibility(t *testing.T) {
	s, _ := hostDrainPolicyFixture(t, true)
	binding := s.binding("ready")
	owner := errors.New("retained cleanup graph")
	graph := errors.Join(ErrCleanupUnresolved, ErrIncompatible, owner)
	control := s.child.control
	if err := reportChildFailure(control, binding, graph); !errors.Is(err, owner) || !errors.Is(err, ErrCleanupUnresolved) || !errors.Is(err, ErrIncompatible) {
		t.Fatalf("report lost original graph: %v", err)
	}
	if err := control.stopSending(); err != nil {
		t.Fatal(err)
	}
	input := bufio.NewReader(control.reader)
	var frame childFrame
	if err := readFrame(input, &frame); err != nil {
		t.Fatal(err)
	}
	expected := binding
	expected.Kind = "cleanup_unresolved"
	if frame != expected {
		t.Fatalf("wrong terminal binding/code: %+v", frame)
	}
	if err := readFrame(input, &frame); !errors.Is(err, io.EOF) {
		t.Fatalf("child sent more than one terminal frame: %v", err)
	}
}

func TestHostCleanupFrameWriteFailurePreservesGraph(t *testing.T) {
	s, _ := hostDrainPolicyFixture(t, true)
	control := s.child.control
	if err := control.stopSending(); err != nil {
		t.Fatal(err)
	}
	owner := errors.New("cleanup remains owned")
	err := reportChildFailure(control, s.binding("ready"), errors.Join(ErrCleanupUnresolved, ErrIncompatible, owner))
	for _, cause := range []error{ErrCleanupUnresolved, ErrIncompatible, owner, os.ErrClosed} {
		if !errors.Is(err, cause) {
			t.Fatalf("failed frame lost cause %v: %v", cause, err)
		}
	}
}
