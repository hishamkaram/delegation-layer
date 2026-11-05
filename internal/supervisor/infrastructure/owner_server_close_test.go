package infrastructure

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOwnerServerConnectionCloseAndMapRemovalAreOneLockedPhase(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	connection := manuallyAcceptOwnerServerConnection(t, f)
	if !f.server.registerConnection(connection) {
		t.Fatal("fixture registration refused")
	}
	closed := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f.server.closeConnection = func(conn *net.UnixConn) error {
		err := conn.Close() // Real native Close, then gate its map-owner return.
		close(closed)
		<-release
		return err
	}
	finished := make(chan struct{})
	var joinOnce sync.Once
	join := func() {
		joinOnce.Do(func() {
			select {
			case <-finished:
			case <-time.After(10 * time.Second):
				f.retain = true
				t.Error("connection finisher did not join")
			}
		})
	}
	t.Cleanup(func() { unblock(); join() })
	go func() { f.server.finishConnection(connection); close(finished) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("native connection Close did not execute")
	}
	if f.server.mu.TryLock() {
		f.server.mu.Unlock()
		t.Fatal("closed registered connection was visible outside its owner lock")
	}
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drained <- f.server.stopAcceptAndDrain(ctx)
	}()
	unblock()
	join()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("drain mistook owner close ordering for a cleanup fault: %v", err)
		}
	case <-time.After(10 * time.Second):
		f.retain = true
		t.Fatal("drain did not join")
	}
}

func TestOwnerServerRejectedConnectionsRaceDrainWithoutCleanupFault(t *testing.T) {
	for _, kind := range []string{"refused", "busy", "auth-rejected"} {
		t.Run(kind, func(t *testing.T) {
			for iteration := range 8 {
				t.Run(strconv.Itoa(iteration), func(t *testing.T) { exerciseOwnerServerRejectedDrain(t, kind) })
			}
		})
	}
}

func exerciseOwnerServerRejectedDrain(t *testing.T, kind string) {
	t.Helper()
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	request := f.request(t, ownerEnsureOwn)
	expected := ownerRefused
	switch kind {
	case "refused":
		request.Context = LaunchContextReference{SHA256: strings.Repeat("f", 64)}
	case "busy":
		if err := f.server.pauseOperationsAndDrain(t.Context()); err != nil {
			t.Fatal(err)
		}
		expected = ownerBusy
	case "auth-rejected":
		request.Client.PID++
	}
	closed := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f.server.closeConnection = func(connection *net.UnixConn) error { err := connection.Close(); close(closed); <-release; return err }
	t.Cleanup(unblock)
	f.start()
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	if reply := readServerReply(t, connection); reply.Code != expected {
		t.Fatalf("%s code=%s want=%s", kind, reply.Code, expected)
	}
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("rejected native connection did not close")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	attempted := make(chan struct{})
	drained := make(chan error, 1)
	go func() { close(attempted); drained <- f.server.stopAcceptAndDrain(ctx) }()
	<-attempted
	unblock()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("%s close/drain produced a false cleanup fault: %v", kind, err)
		}
	case <-ctx.Done():
		f.retain = true
		t.Fatal(ctx.Err())
	}
}
