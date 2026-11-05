package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

const (
	daemonWaitHelperAddress = "PUEUE_TEST_DAEMON_WAIT_ADDRESS"
	daemonWaitHelperFailure = "PUEUE_TEST_DAEMON_WAIT_FAILURE"
)

type daemonWaitOwnerFunc func(string, int, <-chan struct{}) error

func (f daemonWaitOwnerFunc) RegisterDaemonWait(root string, pid int, done <-chan struct{}) error {
	return f(root, pid, done)
}

// This is a finite owned child, not a native resident/placement authority proof.
func TestDaemonWaitHelper(t *testing.T) {
	address := os.Getenv(daemonWaitHelperAddress)
	if address == "" {
		return
	}
	connection, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := connection.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Fprintf(connection, "%d\n", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	var gate [1]byte
	if _, err = connection.Read(gate[:]); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if os.Getenv(daemonWaitHelperFailure) == "1" {
		t.Fatal("finite daemon exit failure fixture")
	}
}

type daemonWaitFixture struct {
	nativeStarted    bool
	root             string
	executable       string
	digest           string
	listener         *net.TCPListener
	connection       net.Conn
	done             <-chan struct{}
	pid              int
	registrations    int
	registrationRoot string
	registrationPID  int
}

func newDaemonWaitFixture(t *testing.T) *daemonWaitFixture {
	t.Helper()
	root, err := os.MkdirTemp("", "dw-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &daemonWaitFixture{root: root}
	t.Cleanup(func() { fixture.close(t) })
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	fixture.listener = listener
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture.executable = filepath.Join(fixture.root, "daemon")
	source := "#!/bin/sh\nexec '" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "' -test.run '^TestDaemonWaitHelper$' -test.timeout 10s\n"
	writeExecutable(t, fixture.executable, source)
	fixture.digest, err = hashExecutable(fixture.executable)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *daemonWaitFixture) options(registrationErr error) Options {
	return Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, Environment: append(os.Environ(), daemonWaitHelperAddress+"="+f.listener.Addr().String()), DaemonWaitOwner: daemonWaitOwnerFunc(func(root string, pid int, done <-chan struct{}) error {
		f.registrations++
		f.registrationRoot, f.registrationPID, f.done = root, pid, done
		return registrationErr
	})}
}

func (f *daemonWaitFixture) start(ctx context.Context, options Options) error {
	guard := options.DaemonStartGuard
	options.DaemonStartGuard = daemonStartGuardFunc(func(startContext context.Context, root string, start func() error) error {
		return guard.GuardDaemonStart(startContext, root, func() error {
			err := start()
			if err == nil {
				f.nativeStarted = true
			}
			return err
		})
	})
	return startDaemon(ctx, f.root, f.executable, f.digest, filepath.Join(f.root, "config"), f.root, options)
}

func (f *daemonWaitFixture) ready(t *testing.T) {
	t.Helper()
	if err := f.listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	connection, err := f.listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	f.connection = connection
	if err = connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	f.pid, err = strconv.Atoi(strings.TrimSpace(line))
	if err != nil || f.pid <= 0 {
		t.Fatalf("invalid ready PID: %v", err)
	}
}

func (f *daemonWaitFixture) release(t *testing.T) {
	t.Helper()
	if f.connection != nil {
		if err := f.connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
			return
		}
		f.connection = nil
	}
}

func (f *daemonWaitFixture) close(t *testing.T) {
	t.Helper()
	f.release(t)
	if f.listener != nil {
		if err := f.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("retain daemon fixture %s: listener close: %v", f.root, err)
			return
		}
	}
	if f.nativeStarted && f.done == nil {
		t.Errorf("retain daemon fixture %s: PID %d completion registration missing", f.root, f.pid)
		return
	}
	if f.done != nil && !joinDaemonWait(t, f.done) {
		t.Errorf("retain daemon fixture %s: PID %d Wait unresolved", f.root, f.registrationPID)
		return
	}
	if f.connection != nil {
		t.Errorf("retain daemon fixture %s: connection closure unresolved", f.root)
		return
	}
	if err := os.RemoveAll(f.root); err != nil {
		t.Error(err)
	}
}

func joinDaemonWait(t *testing.T, done <-chan struct{}) bool {
	t.Helper()
	select {
	case <-done:
		return true
	case <-time.After(10 * time.Second):
		t.Error("owned daemon Wait did not finish")
		return false
	}
}

func TestDaemonWaitRegistrationAndCompletion(t *testing.T) {
	postStart := errors.New("guard completion failed")
	registration := errors.New("wait registration failed")
	for _, tc := range []struct {
		name            string
		guardErr        error
		registrationErr error
	}{
		{name: "success"}, {name: "post-start guard error", guardErr: postStart}, {name: "registration error", registrationErr: registration}, {name: "both errors", guardErr: postStart, registrationErr: registration},
	} {
		t.Run(tc.name, func(t *testing.T) { exerciseDaemonWaitRegistration(t, tc.guardErr, tc.registrationErr) })
	}
}

func exerciseDaemonWaitRegistration(t *testing.T, guardErr, registrationErr error) *daemonWaitFixture {
	t.Helper()
	f := newDaemonWaitFixture(t)
	options := f.options(registrationErr)
	options.DaemonStartGuard = daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
		if err := start(); err != nil {
			return err
		}
		return guardErr
	})
	err := f.start(t.Context(), options)
	requireDaemonWaitLaunchErrors(t, err, guardErr, registrationErr)
	f.ready(t)
	if f.registrations != 1 || f.registrationRoot != f.root || f.registrationPID != f.pid || f.done == nil {
		t.Fatalf("wait registration differs: count=%d root=%t pid=%d want=%d", f.registrations, f.registrationRoot == f.root, f.registrationPID, f.pid)
	}
	reference, err := lifetime.ObserveProcess(f.pid)
	if err != nil {
		t.Fatal(err)
	}
	present, err := lifetime.ProcessPresent(reference)
	if err != nil || !present {
		t.Fatalf("ready child is not alive: %v", err)
	}
	select {
	case <-f.done:
		t.Fatal("Wait completion closed while gated child is alive")
	default:
	}
	f.release(t)
	joinDaemonWait(t, f.done)
	return f
}

func requireDaemonWaitLaunchErrors(t *testing.T, err, guardErr, registrationErr error) {
	t.Helper()
	if guardErr == nil && registrationErr == nil && err != nil {
		t.Fatal(err)
	}
	if guardErr != nil && !errors.Is(err, guardErr) || registrationErr != nil && !errors.Is(err, registrationErr) {
		t.Fatalf("launch errors lost: %v", err)
	}
}

func TestDaemonWaitDoesNotRegisterFailedStart(t *testing.T) {
	f := newDaemonWaitFixture(t)
	options := f.options(nil)
	options.DaemonStartGuard = daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
		if err := os.Remove(f.executable); err != nil {
			return err
		}
		return start()
	})
	if err := f.start(t.Context(), options); !errors.Is(err, os.ErrNotExist) || !errors.Is(err, ErrBinding) {
		t.Fatalf("start failure cause lost: %v", err)
	}
	if f.registrations != 0 || f.done != nil {
		t.Fatal("failed Start registered a completion handle")
	}
}

func TestDaemonWaitClosesAfterChildExitError(t *testing.T) {
	f := newDaemonWaitFixture(t)
	options := f.options(nil)
	options.Environment = append(options.Environment, daemonWaitHelperFailure+"=1")
	if err := f.start(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	f.ready(t)
	f.release(t)
	joinDaemonWait(t, f.done)
}
