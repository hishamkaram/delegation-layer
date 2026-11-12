//go:build darwin

package control

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type darwinEndpointPeerTestResult struct {
	accepted bool
	err      error
}

func serveDarwinEndpointPeerTest(listener *net.UnixListener, deadline time.Time) (result darwinEndpointPeerTestResult) {
	connection, err := listener.AcceptUnix()
	if err != nil {
		return darwinEndpointPeerTestResult{err: err}
	}
	result.accepted = true
	defer func() { result.err = errors.Join(result.err, connection.Close()) }()
	if err = connection.SetReadDeadline(deadline); err != nil {
		result.err = err
		return result
	}
	var data [1]byte
	count, readErr := connection.Read(data[:])
	if count != 0 || !errors.Is(readErr, io.EOF) {
		result.err = errors.Join(errors.New("endpoint observer did not close without sending data"), readErr)
	}
	return result
}

// The fixture owns its listener, one accepted connection and the sole serving
// goroutine. served is published once by closing done; readers join before use.
type darwinEndpointPeerFixture struct {
	directory string
	path      string
	listener  *net.UnixListener
	done      chan struct{}
	served    darwinEndpointPeerTestResult
	started   bool
}

func newDarwinEndpointPeerFixture(t *testing.T, deadline time.Time) *darwinEndpointPeerFixture {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "pep-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &darwinEndpointPeerFixture{directory: directory, done: make(chan struct{})}
	t.Cleanup(func() { fixture.close(t) })
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	fixture.path = filepath.Join(resolved, "s")
	fixture.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: fixture.path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.listener.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	fixture.started = true
	go func() {
		defer close(fixture.done)
		fixture.served = serveDarwinEndpointPeerTest(fixture.listener, deadline)
	}()
	return fixture
}

func (f *darwinEndpointPeerFixture) close(t *testing.T) {
	t.Helper()
	retained := false
	if f.listener != nil {
		if closeErr := f.listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			retained = true
			t.Error(closeErr)
		}
	}
	if f.started {
		cleanup, stop := context.WithTimeout(context.Background(), nativePrivateObservationTimeout)
		defer stop()
		select {
		case <-f.done:
			if f.served.accepted && f.served.err != nil {
				retained = true
				t.Errorf("endpoint peer connection cleanup: %v", f.served.err)
			}
		case <-cleanup.Done():
			retained = true
			t.Error("endpoint peer fixture did not join")
		}
	}
	if retained {
		t.Logf("retained endpoint peer fixture after unresolved closure: %s", f.directory)
		return
	}
	if err := os.RemoveAll(f.directory); err != nil {
		t.Error(err)
	}
}

// This native test's server is the current process, so PID and effective UID
// come from an independent oracle. It also verifies no protocol bytes are sent.
func TestObserveUnixEndpointPeerDarwinNativeCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), nativePrivateObservationTimeout)
	defer cancel()
	deadline, finite := ctx.Deadline()
	if !finite {
		t.Fatal("peer observation requires a finite context")
	}
	fixture := newDarwinEndpointPeerFixture(t, deadline)
	peer, err := observeUnixEndpointPeer(ctx, fixture.path)
	if err != nil {
		t.Fatalf("native private endpoint peer unverified: %v", err)
	}
	if peer.PID != os.Getpid() || peer.UID != os.Geteuid() {
		t.Fatal("kernel endpoint peer does not identify the fixture process and effective user")
	}
	select {
	case <-fixture.done:
		if fixture.served.err != nil {
			t.Fatalf("endpoint peer connection: %v", fixture.served.err)
		}
	case <-ctx.Done():
		t.Fatal("endpoint peer observer did not close its owned connection")
	}
}

func TestObserveUnixEndpointPeerDarwinRefusesInvalidCalls(t *testing.T) {
	path := filepath.Join(canonicalTemp(t), "missing")
	finite, stop := context.WithTimeout(context.Background(), nativePrivateObservationTimeout)
	defer stop()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		cause   error
		binding bool
	}{
		{name: "nil_context", cause: context.Canceled},
		{name: "canceled_context", ctx: canceled, cause: context.Canceled, binding: true},
		{name: "missing_endpoint", ctx: finite, cause: os.ErrNotExist, binding: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer, err := observeUnixEndpointPeer(tc.ctx, path)
			if peer != (privateEndpointPeer{}) || !errors.Is(err, tc.cause) {
				t.Fatalf("unverified endpoint returned a peer or lost its cause: %v", err)
			}
			if tc.binding && !errors.Is(err, ErrBinding) {
				t.Fatalf("endpoint failure lost binding classification: %v", err)
			}
		})
	}
}

func TestDarwinUnixConnectionPeerRejectsInvalidConnection(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		if err := errors.Join(client.Close(), server.Close()); err != nil {
			t.Error(err)
		}
	})
	if peer, err := unixConnectionPeer(client); peer != (privateEndpointPeer{}) || !errors.Is(err, ErrBinding) {
		t.Fatalf("non-Unix connection accepted: %v", err)
	}
	var connection *net.UnixConn
	peer, err := unixConnectionPeer(connection)
	if peer != (privateEndpointPeer{}) || !errors.Is(err, ErrBinding) || !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("native reader failure returned a peer or lost its original cause: %v", err)
	}
}
