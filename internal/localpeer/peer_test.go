//go:build linux || darwin

package localpeer

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func connectedPeers(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	// Reuse the native host fixtures' short HOME geometry, including on Darwin
	// where the caller's default TMPDIR may leave no room for a Unix socket.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, ".lp-")
	if err != nil {
		t.Fatal(err)
	}
	cleanupFailed := false
	t.Cleanup(func() {
		if cleanupFailed {
			t.Logf("retained peer fixture directory after unresolved closure: %s", directory)
			return
		}
		if removeErr := os.RemoveAll(directory); removeErr != nil {
			t.Error(removeErr)
		}
	})
	address := &net.UnixAddr{Name: filepath.Join(directory, "s"), Net: "unix"}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	registerPeerCleanup(t, &cleanupFailed, listener.Close)
	if err = listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	client, err := net.DialUnix("unix", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	registerPeerCleanup(t, &cleanupFailed, client.Close)
	server, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	registerPeerCleanup(t, &cleanupFailed, server.Close)
	return client, server
}

func registerPeerCleanup(t *testing.T, failed *bool, closePeer func() error) {
	t.Helper()
	t.Cleanup(func() {
		if err := closePeer(); err != nil && !errors.Is(err, net.ErrClosed) {
			*failed = true
			t.Error(err)
		}
	})
}

func TestReadNativeConnectedPeerCredentials(t *testing.T) {
	client, server := connectedPeers(t)
	for _, conn := range []*net.UnixConn{client, server} {
		credentials, err := Read(conn)
		if err != nil {
			t.Fatal(err)
		}
		if credentials.PID != os.Getpid() || credentials.UID != os.Geteuid() {
			t.Fatal("kernel peer observation did not match this fixture's process")
		}
	}
}

func TestReadRefusesNilAndClosedConnections(t *testing.T) {
	if credentials, err := Read(nil); !errors.Is(err, os.ErrInvalid) || credentials != (Credentials{}) {
		t.Fatalf("nil connection accepted: %v", err)
	}
	client, _ := connectedPeers(t)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if credentials, err := Read(client); !errors.Is(err, net.ErrClosed) || credentials != (Credentials{}) {
		t.Fatalf("closed connection accepted: %v", err)
	}
}

func TestNativeCredentialsRefuseInvalidFacts(t *testing.T) {
	for _, input := range []struct {
		pid int
		uid uint32
	}{{0, 0}, {-1, 0}, {1, ^uint32(0)}} {
		if credentials, err := nativeCredentials(input.pid, input.uid); !errors.Is(err, ErrCredentials) || credentials != (Credentials{}) {
			t.Fatalf("invalid native facts accepted: %v", err)
		}
	}
}
