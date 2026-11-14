//go:build linux

package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

type privatePortalFixture struct {
	directory string
	address   string
	ctx       context.Context
	cancel    context.CancelFunc
	command   *exec.Cmd
	done      chan struct{}
	waitErr   error
	server    *portalConnection
	foreign   *portalConnection
	portal    *portalFixture
}

func newPrivatePortalFixture(t *testing.T, mode string) *privatePortalFixture {
	t.Helper()
	directory, err := os.MkdirTemp("", "delegate-portal-")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	f := &privatePortalFixture{directory: directory, address: filepath.Join(directory, "bus"), ctx: ctx, cancel: cancel}
	t.Cleanup(func() { f.close(t) })
	configuration := filepath.Join(directory, "bus.conf")
	xml := fmt.Sprintf(`<busconfig><type>session</type><listen>unix:path=%s</listen><auth>EXTERNAL</auth><policy context="default"><allow own="*"/><allow send_destination="*"/><allow receive_sender="*"/></policy></busconfig>`, f.address)
	if err = os.WriteFile(configuration, []byte(xml), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := verifiedexec.LookPath("dbus-daemon", os.Environ())
	if err != nil {
		t.Fatalf("private protocol fixture requires installed dbus-daemon: %v", err)
	}
	f.command = exec.Command(executable, "--nofork", "--config-file", configuration)
	f.command.Env = []string{"PATH=/usr/bin:/bin"}
	f.command.Dir = directory
	owner, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.done = make(chan struct{})
	// The public owner API reports verified deadline teardown. Explicit early
	// cancellation is deliberately not interpreted as fixture closure proof.
	// testing.T cancels its Context before Cleanup. This bounded owner deadline
	// must survive that transition so cleanup observes Native's expiry proof.
	brokerContext, brokerCancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
	go func() {
		defer brokerCancel()
		f.waitErr = execution.RunIsolatedCommandNullOutput(brokerContext, owner, f.command)
		close(f.done)
	}()
	waitPrivateBus(t, f)
	f.server, err = connectPortal(ctx, f.address)
	if err != nil {
		t.Fatal(err)
	}
	f.foreign, err = connectPortal(ctx, f.address)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := f.server.conn.RequestName(portalName, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("private portal name: %v %v", reply, err)
	}
	f.portal = &portalFixture{connection: f.server.conn, foreign: f.foreign.conn, mode: mode}
	if err = f.server.conn.Export(f.portal, portalObject, "org.freedesktop.portal.OpenURI"); err != nil {
		t.Fatal(err)
	}
	return f
}

func waitPrivateBus(t *testing.T, f *privatePortalFixture) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if info, err := os.Stat(f.address); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		select {
		case <-f.ctx.Done():
			t.Fatal("private bus startup deadline")
		case <-f.done:
			t.Fatal("private bus exited before readiness")
		case <-ticker.C:
		}
	}
}

func (f *privatePortalFixture) close(t *testing.T) {
	t.Helper()
	defer f.cancel()
	var cleanup error
	for _, connection := range []*portalConnection{f.foreign, f.server} {
		if connection != nil {
			cleanup = errors.Join(cleanup, connection.close(f.ctx))
		}
	}
	if f.done != nil {
		select {
		case <-f.done:
		case <-time.After(35 * time.Second):
			t.Errorf("private broker join unconfirmed; retained %s", f.directory)
			return
		}
		if f.waitErr != nil && !execution.NativeCommandTimedOut(f.waitErr) {
			cleanup = errors.Join(cleanup, f.waitErr)
		}
	}
	if cleanup != nil {
		t.Errorf("private portal connection cleanup failed: %v; retained %s", cleanup, f.directory)
		return
	}
	if err := os.RemoveAll(f.directory); err != nil {
		t.Error(err)
	}
}

type portalFixture struct {
	connection *dbus.Conn
	foreign    *dbus.Conn
	mode       string
	mu         sync.Mutex
	uri        string
	closed     atomic.Int32
}

func (p *portalFixture) OpenURI(sender dbus.Sender, parent, uri string, options map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	value, supplied := options["handle_token"]
	token, valid := value.Value().(string)
	if !supplied || !valid || parent != "" {
		return "", dbus.MakeFailedError(errors.New("invalid portal request"))
	}
	p.mu.Lock()
	p.uri = uri
	p.mu.Unlock()
	name := strings.ReplaceAll(strings.TrimPrefix(string(sender), ":"), ".", "_")
	handle := dbus.ObjectPath(requestPrefix + name + "/" + token)
	if err := p.connection.Export(&portalRequestFixture{closed: &p.closed}, handle, requestResponse); err != nil {
		return "", dbus.MakeFailedError(err)
	}
	if p.mode != "missing-response" {
		if err := p.emit(handle); err != nil {
			return "", dbus.MakeFailedError(err)
		}
	}
	if p.mode == "wrong-handle" {
		return dbus.ObjectPath(string(handle) + "/foreign"), nil
	}
	return handle, nil
}

func (p *portalFixture) emit(path dbus.ObjectPath) error {
	connection := p.connection
	var code any = uint32(0)
	switch p.mode {
	case "wrong-sender":
		connection = p.foreign
	case "wrong-path":
		path = "/foreign/request"
	case "wrong-body":
		code = "not-a-response-code"
	case "cancelled":
		code = uint32(1)
	}
	// Intentionally emit BEFORE the method reply to protect subscribe ordering.
	return connection.Emit(path, responseSignal, code, map[string]dbus.Variant{})
}

type portalRequestFixture struct{ closed *atomic.Int32 }

func (p *portalRequestFixture) Close() *dbus.Error { p.closed.Add(1); return nil }

func TestPrivatePortalRequiresAuthenticatedExactResponse(t *testing.T) {
	for _, mode := range []string{"success", "missing-response", "wrong-sender", "wrong-path", "wrong-body", "wrong-handle", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newPrivatePortalFixture(t, mode)
			budget := 3 * time.Second
			if mode == "missing-response" || mode == "wrong-sender" || mode == "wrong-path" {
				budget = 200 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(f.ctx, budget)
			defer cancel()
			environment := []string{"DBUS_SESSION_BUS_ADDRESS=unix:path=" + dbus.EscapeBusAddressValue(f.address)}
			before := os.Environ()
			err := openPortal(ctx, environment, "https://issuer.example/activate")
			if (err == nil) != (mode == "success") {
				t.Fatalf("portal %s: %v", mode, err)
			}
			if !reflect.DeepEqual(before, os.Environ()) {
				t.Fatal("portal activation mutated process environment")
			}
			f.portal.mu.Lock()
			uri := f.portal.uri
			f.portal.mu.Unlock()
			if uri != "https://issuer.example/activate" {
				t.Fatalf("unexpected URI: %q", uri)
			}
			if (mode == "wrong-body" || mode == "wrong-handle") && f.portal.closed.Load() != 1 {
				t.Fatal("failed owned request was not closed before connection cleanup")
			}
		})
	}
}

func TestPortalAuthenticationCancellationJoinsRawSocket(t *testing.T) {
	f := newStalledAuthentication(t)
	client, err := connectPortal(f.ctx, f.listener.Addr().String())
	if client != nil {
		if closeErr := client.close(f.ctx); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if err == nil || client != nil {
		t.Fatal("stalled authentication was accepted")
	}
}

type stalledAuthentication struct {
	directory string
	listener  net.Listener
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	err       error
}

func newStalledAuthentication(t *testing.T) *stalledAuthentication {
	t.Helper()
	directory, err := os.MkdirTemp("", "delegate-portal-stall-")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	f := &stalledAuthentication{directory: directory, ctx: ctx, cancel: cancel}
	t.Cleanup(func() { f.close(t) })
	f.listener, err = net.Listen("unix", filepath.Join(directory, "bus"))
	if err != nil {
		t.Fatal(err)
	}
	f.done = make(chan struct{})
	go func() {
		f.err = f.run()
		close(f.done)
	}()
	return f
}

func (f *stalledAuthentication) run() (result error) {
	peer, err := f.listener.Accept()
	if err != nil {
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
	defer func() { result = errors.Join(result, peer.Close()) }()
	deadline, bounded := f.ctx.Deadline()
	if !bounded {
		return errors.New("private peer requires a deadline")
	}
	if err = peer.SetDeadline(deadline); err != nil {
		return err
	}
	var first [1]byte
	if _, err = io.ReadFull(peer, first[:]); err != nil {
		return err
	}
	f.cancel()
	for {
		if _, err = peer.Read(first[:]); err != nil {
			if errors.Is(err, io.EOF) || f.ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func (f *stalledAuthentication) close(t *testing.T) {
	t.Helper()
	f.cancel()
	if f.listener != nil {
		if err := f.listener.Close(); err != nil {
			t.Error(err)
		}
	}
	if f.done != nil {
		select {
		case <-f.done:
			if f.err != nil {
				t.Errorf("stalled peer cleanup failed: %v; retained %s", f.err, f.directory)
				return
			}
		case <-time.After(2 * time.Second):
			t.Errorf("stalled peer join unconfirmed; retained %s", f.directory)
			return
		}
	}
	if err := os.RemoveAll(f.directory); err != nil {
		t.Error(err)
	}
}

type observedPortalWrite struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *observedPortalWrite) Write(data []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(data)
}

func TestPortalCancellationBreaksBlockedGodbusOutputLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	local, peer := net.Pipe()
	observed := &observedPortalWrite{Conn: local, started: make(chan struct{})}
	client, err := configurePortalConnection(ctx, observed)
	if err != nil {
		cancel()
		if peerErr := peer.Close(); peerErr != nil {
			t.Error(peerErr)
		}
		t.Fatal(err)
	}
	done := make(chan struct{})
	var callErr error
	t.Cleanup(func() {
		cancel()
		if peerErr := peer.Close(); peerErr != nil {
			t.Error(peerErr)
		}
		if closeErr := client.close(ctx); closeErr != nil {
			t.Error(closeErr)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("blocked D-Bus writer join unconfirmed")
		}
	})
	go func() {
		callErr = client.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.Hello", 0).Err
		close(done)
	}()
	select {
	case <-observed.started:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not reach transport")
	}
	cancel()
	select {
	case <-done:
		if callErr == nil {
			t.Fatal("blocked write was accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation could not break D-Bus output lock")
	}
}
