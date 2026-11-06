package infrastructure

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOwnerServerRequestDiagnosticDoesNotPoisonClosure(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	first := errors.New("untrusted request path must remain redacted")
	f.server.connectionFault(first)
	f.server.connectionFault(io.EOF)
	observed := f.server.diagnostic()
	if !errors.Is(observed, first) || strings.Contains(observed.Error(), first.Error()) {
		t.Fatalf("first bounded diagnostic lost or exposed: %v", observed)
	}
	if err := f.server.stopAcceptAndDrain(t.Context()); err != nil {
		t.Fatalf("diagnostic became closure fault: %v", err)
	}
	if !errors.Is(f.server.diagnostic(), first) {
		t.Fatal("drain erased diagnostic")
	}
}

func TestOwnerServerInjectedConnectionCloseFaultRemainsClosureFault(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	server := manuallyAcceptOwnerServerConnection(t, f)
	if !f.server.registerConnection(server) {
		t.Fatal("fixture connection not registered")
	}
	injected := errors.New("injected native close result")
	f.server.closeConnection = func(connection *net.UnixConn) error {
		if err := connection.Close(); err != nil {
			return err
		}
		// The fixture has proved real native closure before injecting a reported
		// failure. Production must still retain that failure as unresolved.
		return injected
	}
	f.expectedCleanupFault = injected
	f.server.finishConnection(server)
	f.server.connectionFault(io.EOF)
	if err := f.server.stopAcceptAndDrain(t.Context()); !errors.Is(err, injected) {
		t.Fatalf("close fault erased or replaced by disconnect: %v", err)
	}
}

func TestOwnerServerInjectedDeadlineWakeFaultRemainsClosureFault(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	server := manuallyAcceptOwnerServerConnection(t, f)
	if !f.server.registerConnection(server) {
		t.Fatal("fixture connection not registered")
	}
	injected := errors.New("injected native deadline wake failure")
	calls := 0
	f.server.wakeConnection = func(connection *net.UnixConn, _ time.Time) error {
		raw, err := connection.SyscallConn()
		if err != nil {
			return err
		}
		open := false
		if err = raw.Control(func(uintptr) { open = true }); err != nil {
			return err
		}
		if !open {
			return ErrIdentity
		}
		calls++
		return injected
	}
	f.expectedCleanupFault = injected
	f.server.stopAccepting() // Fault on an actually OPEN registered native FD.
	f.server.finishConnection(server)
	if err := f.server.stopAcceptAndDrain(t.Context()); !errors.Is(err, injected) {
		t.Fatalf("wake fault erased: %v", err)
	}
	if calls != 1 {
		t.Fatalf("native wake attempts=%d", calls)
	}
}

func manuallyAcceptOwnerServerConnection(t *testing.T, f *ownerServerFixture) *net.UnixConn {
	t.Helper()
	f.dial(t)
	if err := f.server.listener.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	server, err := f.server.listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.server.mu.Lock()
		_, registered := f.server.connections[server]
		f.server.mu.Unlock()
		if registered {
			f.server.finishConnection(server)
			return
		}
		// Unregistered excess connections belong only to this fixture. A completed
		// finisher may already have proved closure; never reinsert a closed FD.
		if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	return server
}

func TestOwnerServerNativePreauthCapIsIndependentAndBounded(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	for range ownerPreauthLimit {
		connection := manuallyAcceptOwnerServerConnection(t, f)
		if !f.server.registerConnection(connection) {
			t.Fatal("pre-auth slot refused before limit")
		}
	}
	excess := manuallyAcceptOwnerServerConnection(t, f)
	if f.server.registerConnection(excess) {
		t.Fatal("pre-auth cap admitted excess physical connection")
	}
	f.server.mu.Lock()
	preauth := f.server.connectionCount(ownerConnectionPreauth)
	total := len(f.server.connections)
	f.server.mu.Unlock()
	if preauth != ownerPreauthLimit || total != ownerPreauthLimit {
		t.Fatalf("unbounded pre-auth actors: %d/%d", preauth, total)
	}
}

func TestOwnerServerReservedControlCapacityRemainsBounded(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	request := f.request(t, ownerStatus)
	for range ownerControlLimit {
		connection := manuallyAcceptOwnerServerConnection(t, f)
		if !f.server.registerConnection(connection) {
			t.Fatal("fixture pre-auth registration refused")
		}
		if _, err := f.server.acceptWork(t.Context(), connection, request); err != nil {
			t.Fatal(err)
		}
	}
	excess := manuallyAcceptOwnerServerConnection(t, f)
	if !f.server.registerConnection(excess) {
		t.Fatal("pre-auth unexpectedly counted admitted controls")
	}
	if _, err := f.server.acceptWork(t.Context(), excess, request); !errors.Is(err, errOwnerWorkBusy) {
		t.Fatalf("control capacity admitted excess: %v", err)
	}
	f.server.mu.Lock()
	control := f.server.connectionCount(ownerConnectionControl)
	preauth := f.server.connectionCount(ownerConnectionPreauth)
	f.server.mu.Unlock()
	if control != ownerControlLimit || preauth != 1 {
		t.Fatalf("control/pre-auth counts=%d/%d", control, preauth)
	}
}

func TestOwnerServerBusyWriteRetainsExpiredOriginalDeadline(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	server := manuallyAcceptOwnerServerConnection(t, f)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Nanosecond))
	defer cancel()
	if err := f.server.setBusyDeadline(server, ctx); err != nil {
		t.Fatal(err)
	}
	reply := ownerServerReply(f.request(t, ownerEnsureOwn), ownerBusy)
	if err := writeOwnerFrame(server, reply); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Busy write renewed expired request budget: %v", err)
	}
}
