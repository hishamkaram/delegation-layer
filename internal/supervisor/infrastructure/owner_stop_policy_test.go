package infrastructure

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
)

func TestOwnerStopAndOperationAdmissionAreAtomic(t *testing.T) {
	for _, order := range []string{"admit-first", "stop-first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			f := newOwnerServerFixture(t, &ownerServerTestHost{})
			if _, err := f.server.prepareInitial(t.Context()); err != nil {
				t.Fatal(err)
			}
			connection := manuallyAcceptOwnerServerConnection(t, f)
			if !f.server.registerConnection(connection) {
				t.Fatal("native fixture connection refused")
			}
			var admitted error
			var stopped ownerStopCode
			switch order {
			case "admit-first":
				admitted = f.server.admitConnection(connection, ownerEnsureOwn)
				stopped = f.server.commitOwnerStop()
			case "stop-first":
				stopped = f.server.commitOwnerStop()
				admitted = f.server.admitConnection(connection, ownerEnsureOwn)
			default:
				start := make(chan struct{})
				admissionDone := make(chan error, 1)
				stopDone := make(chan ownerStopCode, 1)
				go func() { <-start; admissionDone <- f.server.admitConnection(connection, ownerEnsureOwn) }()
				go func() { <-start; code := f.server.commitOwnerStop(); stopDone <- code }()
				close(start)
				admitted, stopped = <-admissionDone, <-stopDone
			}
			if stopped == ownerStopAccepted {
				if !errors.Is(admitted, os.ErrClosed) {
					t.Fatalf("committed stop overlapped admitted work: %v", admitted)
				}
			} else if stopped != ownerStopBusy || admitted != nil || f.server.isClosing() {
				t.Fatalf("active operation changed stop state: code=%s admission=%v", stopped, admitted)
			}
			f.server.finishConnection(connection)
		})
	}
}

func TestOwnerStopRetainsStartToWaitRegistrationSlot(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	connection := manuallyAcceptOwnerServerConnection(t, f)
	if !f.server.registerConnection(connection) {
		t.Fatal("native fixture connection refused")
	}
	if err := f.server.admitConnection(connection, ownerEnsureOwn); err != nil {
		t.Fatal(err)
	}
	if code := f.server.commitOwnerStop(); code != ownerStopBusy || f.server.isClosing() {
		t.Fatal("operation lost custody before Wait registration")
	}
	done := make(chan struct{})
	defer close(done)
	if err := f.server.waits.RegisterDaemonWait(f.root, os.Getpid(), done); err != nil {
		t.Fatal(err)
	}
	f.server.finishConnection(connection)
	code := f.server.commitOwnerStop()
	if code != ownerStopAccepted {
		t.Fatalf("settled operation refused stop: code=%s", code)
	}
	assertOwnerStopRetainsDaemonWait(t, f.server, done)
}

func assertOwnerStopRetainsDaemonWait(t *testing.T, server *ownerServer, done <-chan struct{}) {
	t.Helper()
	server.waits.mu.Lock()
	pid, retained := server.waits.active[done]
	server.waits.mu.Unlock()
	if !retained || pid != os.Getpid() {
		t.Fatalf("stop lost the original daemon completion: retained=%t pid=%d", retained, pid)
	}
}

func TestOwnerStopReportsCleanupFaultAndIgnoresOrdinaryDiagnostic(t *testing.T) {
	for _, cleanupFault := range []bool{false, true} {
		t.Run(map[bool]string{false: "diagnostic", true: "native-close-fault"}[cleanupFault], func(t *testing.T) {
			f := newOwnerServerFixture(t, &ownerServerTestHost{})
			if _, err := f.server.prepareInitial(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.server.connectionFault(io.EOF)
			want := ownerStopAccepted
			if cleanupFault {
				connection := manuallyAcceptOwnerServerConnection(t, f)
				if !f.server.registerConnection(connection) {
					t.Fatal("native fixture connection refused")
				}
				fault := errors.New("native close reported a release fault")
				f.expectedCleanupFault = fault
				f.server.closeConnection = func(connection *net.UnixConn) error { return errors.Join(connection.Close(), fault) }
				f.server.finishConnection(connection)
				want = ownerStopFault
			}
			if code := f.server.commitOwnerStop(); code != want {
				t.Fatalf("stop changed fault classification: got=%s want=%s", code, want)
			}
		})
	}
}
