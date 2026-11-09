//go:build linux || darwin

package lifetime

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShutdownLeaseCleanupKeepsExitGateThroughFailedSettlement(t *testing.T) {
	proof, actor := newShutdownLeaseCleanupGate(t)
	done := make(chan struct{})
	var actorErr error
	go func() { defer close(done); actorErr = runShutdownLeaseCleanupActor(actor) }()
	t.Cleanup(func() {
		closeShutdownLeaseCleanupEndpoint(t, proof.connection)
		closeShutdownLeaseCleanupEndpoint(t, actor)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("exit-gated actor did not join")
		}
	})
	settlementErr := errors.New("actor closure unproven")
	closeErr := proof.closeActorGateAfter(func() error {
		// Byte exchange proves the EOF-driven actor is still held during settlement.
		if _, err := proof.connection.Write([]byte("s")); err != nil {
			t.Errorf("exit gate released before settlement: %v", err)
			return settlementErr
		}
		var reply [1]byte
		if _, err := io.ReadFull(proof.connection, reply[:]); err != nil {
			t.Errorf("actor exited during settlement: %v", err)
		}
		return settlementErr
	})
	if !errors.Is(closeErr, settlementErr) {
		t.Fatalf("lost unresolved actor error: %v", closeErr)
	}
	select {
	case <-done:
		if actorErr != nil {
			t.Fatalf("actor exit: %v", actorErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gate was not released after failed settlement")
	}
	probe, dialErr := net.DialTimeout("unix", proof.listener.Addr().String(), time.Second)
	if dialErr == nil {
		closeShutdownLeaseCleanupEndpoint(t, probe)
		t.Fatal("listener remained open after failed settlement")
	}
}

func runShutdownLeaseCleanupActor(actor net.Conn) error {
	var request [1]byte
	if _, err := io.ReadFull(actor, request[:]); err != nil {
		return err
	}
	if _, err := actor.Write(request[:]); err != nil {
		return err
	}
	_, err := actor.Read(request[:])
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("exit gate sent unexpected data")
	}
	return err
}

func newShutdownLeaseCleanupGate(t *testing.T) (*shutdownLeaseProof, *net.UnixConn) {
	t.Helper()
	base, err := os.MkdirTemp("", "dl-gate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(base); removeErr != nil {
			t.Error(removeErr)
		}
	})
	address := &net.UnixAddr{Name: filepath.Join(base, "s"), Net: "unix"}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeShutdownLeaseCleanupEndpoint(t, listener) })
	actor, err := net.DialUnix("unix", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeShutdownLeaseCleanupEndpoint(t, actor) })
	connection, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeShutdownLeaseCleanupEndpoint(t, connection) })
	deadline := time.Now().Add(5 * time.Second)
	for _, endpoint := range []*net.UnixConn{actor, connection} {
		if err := endpoint.SetDeadline(deadline); err != nil {
			t.Fatal(err)
		}
	}
	return &shutdownLeaseProof{connection: connection, listener: listener}, actor
}

func closeShutdownLeaseCleanupEndpoint(t *testing.T, endpoint io.Closer) {
	t.Helper()
	// The fixture owner may already have closed this endpoint during settlement.
	if err := endpoint.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Error(err)
	}
}
