package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

type daemonBirthWaitOwnerForTest struct {
	legacy          DaemonWaitOwner
	birth           func() (lifetime.ProcessRef, bool, error)
	birthCalls      int
	legacyCalls     int
	registrationErr error
}

func (owner *daemonBirthWaitOwnerForTest) RegisterDaemonWait(root string, pid int, done <-chan struct{}) error {
	owner.legacyCalls++
	return owner.legacy.RegisterDaemonWait(root, pid, done)
}

func (owner *daemonBirthWaitOwnerForTest) RegisterDaemonWaitBirth(root string, pid int, done <-chan struct{}, birth func() (lifetime.ProcessRef, bool, error)) error {
	owner.birthCalls++
	owner.birth = birth
	return errors.Join(owner.legacy.RegisterDaemonWait(root, pid, done), owner.registrationErr)
}

func TestDaemonBirthRegistrationRetainsWaitAfterGuardFault(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("post_start_fault_%t", fail), func(t *testing.T) {
			fixture := newDaemonWaitFixture(t)
			options := fixture.options(nil)
			owner := &daemonBirthWaitOwnerForTest{legacy: options.DaemonWaitOwner}
			options.DaemonWaitOwner = owner
			var guardErr error
			if fail {
				guardErr = errors.New("post-Start guard fault")
				owner.registrationErr = errors.New("birth registration fault")
				options.DaemonStartGuard = daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
					return errors.Join(start(), guardErr)
				})
			}
			startErr := fixture.start(t.Context(), options)
			requireDaemonWaitLaunchErrors(t, startErr, guardErr, owner.registrationErr)
			fixture.ready(t)
			if owner.birthCalls != 1 || owner.legacyCalls != 0 || fixture.registrations() != 1 {
				t.Fatalf("optional registration duplicated/lost ownership: birth=%d legacy=%d actual=%d", owner.birthCalls, owner.legacyCalls, fixture.registrations())
			}
			before, birthErr := awaitDaemonBirth(t, owner.birth)
			if birthErr != nil || before.PID() != fixture.pid() {
				t.Fatalf("registered birth differs: pid=%d err=%v", before.PID(), birthErr)
			}
			fixture.release(t)
			if !joinDaemonWait(t, fixture.done()) {
				return
			}
			after, ready, afterErr := owner.birth()
			same, identityErr := lifetime.SameProcess(before, after)
			if !ready || afterErr != nil || identityErr != nil || !same {
				t.Fatalf("reaped birth was reread/changed: ready=%t err=%v identity=%v", ready, afterErr, identityErr)
			}
		})
	}
}

func awaitDaemonBirth(t *testing.T, birth func() (lifetime.ProcessRef, bool, error)) (lifetime.ProcessRef, error) {
	t.Helper()
	if birth == nil {
		t.Fatal("started child has no birth future")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if reference, ready, err := birth(); ready {
			return reference, err
		}
		select {
		case <-deadline.C:
			t.Fatal("owned child birth observation did not finish")
		case <-ticker.C:
		}
	}
}

func startDaemonBirthTestChild(t *testing.T, fixture *daemonWaitFixture, observe func(int) (lifetime.ProcessRef, error)) (*exec.Cmd, privateDaemonStart) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-test.run=^TestDaemonWaitHelper$", "-test.timeout=10s")
	options := fixture.options(nil)
	command.Env, command.Dir = options.Environment, fixture.root
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	// Establish the sole reaper before any fallible test bookkeeping.
	started := startDaemonWait(command, nil, observe)
	fixture.mu.Lock()
	fixture.nativeStarted = true
	fixture.mu.Unlock()
	if err = options.DaemonWaitOwner.RegisterDaemonWait(fixture.root, command.Process.Pid, started.done); err != nil {
		t.Fatal(err)
	}
	return command, started
}

func TestDaemonBirthCaptureBeforeReapingExitedChild(t *testing.T) {
	fixture := newDaemonWaitFixture(t)
	releaseCapture := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCapture) }) }
	t.Cleanup(release)
	command, started := startDaemonBirthTestChild(t, fixture, func(pid int) (lifetime.ProcessRef, error) {
		<-releaseCapture
		return lifetime.ObserveStartedProcess(pid)
	})
	fixture.ready(t)
	before, err := lifetime.ObserveStartedProcess(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	assertDaemonBirthGetterUnready(t, started.birth, release)
	fixture.release(t)
	awaitDaemonBirthChildExit(t, before)
	select {
	case <-started.done:
		t.Fatal("child was reaped before its birth observation")
	default:
	}
	release()
	got, birthErr := awaitDaemonBirth(t, started.birth)
	same, identityErr := lifetime.SameProcess(before, got)
	if birthErr != nil || identityErr != nil || !same {
		t.Fatalf("unreaped exit lost its birth: err=%v identity=%v same=%t", birthErr, identityErr, same)
	}
	if !joinDaemonWait(t, started.done) {
		return
	}
	if command.ProcessState == nil || !command.ProcessState.Exited() {
		t.Fatal("natural Cmd.Wait completion was not retained")
	}
}

func assertDaemonBirthGetterUnready(t *testing.T, birth func() (lifetime.ProcessRef, bool, error), release func()) {
	t.Helper()
	type result struct {
		reference lifetime.ProcessRef
		ready     bool
		err       error
	}
	returned, joined := make(chan result, 1), make(chan struct{})
	go func() {
		reference, ready, err := birth()
		returned <- result{reference: reference, ready: ready, err: err}
		close(joined)
	}()
	t.Cleanup(func() {
		release()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("birth getter caller did not join")
		}
	})
	select {
	case got := <-returned:
		if got.ready || got.err != nil || got.reference.PID() != 0 {
			t.Fatalf("unobserved birth supplied evidence: ready=%t err=%v", got.ready, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("birth getter waited for its producer")
	}
}

func awaitDaemonBirthChildExit(t *testing.T, reference lifetime.ProcessRef) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		present, err := lifetime.ProcessPresent(reference)
		if err != nil {
			t.Fatal(err)
		}
		if !present {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("released child did not exit before its deferred reap")
		case <-ticker.C:
		}
	}
}

func TestDaemonBirthCaptureFailureCannotWithholdWait(t *testing.T) {
	fixture := newDaemonWaitFixture(t)
	failure := errors.New("birth unavailable")
	command, started := startDaemonBirthTestChild(t, fixture, func(int) (lifetime.ProcessRef, error) {
		return lifetime.ProcessRef{}, failure
	})
	fixture.ready(t)
	fixture.release(t)
	// No consumer reads the future until after the real child has been reaped.
	if !joinDaemonWait(t, started.done) {
		return
	}
	_, ready, err := started.birth()
	if !ready || !errors.Is(err, failure) || command.ProcessState == nil || !command.ProcessState.Exited() {
		t.Fatalf("capture failure replaced/blocked Wait: ready=%t err=%v", ready, err)
	}
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

// children owns every observed child, connection and actual Wait. Accessors
// derive the single-child views used by the original tests.
type daemonWaitChild struct {
	pid        int
	root       string
	done       <-chan struct{}
	connection net.Conn
	handshake  bool
}

type daemonWaitFixture struct {
	mu            sync.Mutex
	nativeStarted bool
	root          string
	executable    string
	listener      *net.TCPListener
	children      []daemonWaitChild
}

func (f *daemonWaitFixture) childLocked(pid int) *daemonWaitChild {
	for index := range f.children {
		if f.children[index].pid == pid {
			return &f.children[index]
		}
	}
	f.children = append(f.children, daemonWaitChild{pid: pid})
	return &f.children[len(f.children)-1]
}

func (f *daemonWaitFixture) pid() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.children) == 0 {
		return 0
	}
	return f.children[len(f.children)-1].pid
}

func (f *daemonWaitFixture) registration() daemonWaitChild {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := len(f.children) - 1; index >= 0; index-- {
		if f.children[index].done != nil {
			return f.children[index]
		}
	}
	return daemonWaitChild{}
}

func (f *daemonWaitFixture) done() <-chan struct{}    { return f.registration().done }
func (f *daemonWaitFixture) registrationPID() int     { return f.registration().pid }
func (f *daemonWaitFixture) registrationRoot() string { return f.registration().root }
func (f *daemonWaitFixture) registrations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, child := range f.children {
		if child.done != nil {
			count++
		}
	}
	return count
}

func newDaemonWaitFixture(t *testing.T) *daemonWaitFixture {
	t.Helper()
	root, err := os.MkdirTemp("", "dw-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &daemonWaitFixture{root: root}
	t.Cleanup(func() { fixture.close(t) })
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	fixture.root = canonicalRoot
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
	return fixture
}

func (f *daemonWaitFixture) options(registrationErr error) Options {
	return Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, Environment: append(os.Environ(), daemonWaitHelperAddress+"="+f.listener.Addr().String()), DaemonWaitOwner: daemonWaitOwnerFunc(func(root string, pid int, done <-chan struct{}) error {
		f.mu.Lock()
		child := f.childLocked(pid)
		child.root, child.done = root, done
		f.mu.Unlock()
		return registrationErr
	})}
}

func (f *daemonWaitFixture) start(ctx context.Context, options Options) error {
	guard := options.DaemonStartGuard
	options.DaemonStartGuard = daemonStartGuardFunc(func(startContext context.Context, root string, start func() error) error {
		return guard.GuardDaemonStart(startContext, root, func() error {
			err := start()
			if err == nil {
				f.mu.Lock()
				f.nativeStarted = true
				f.mu.Unlock()
			}
			return err
		})
	})
	return startDaemon(ctx, f.root, f.executable, filepath.Join(f.root, "config"), f.root, options)
}

func (f *daemonWaitFixture) acceptChild() (err error) {
	if err = f.listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	connection, err := f.listener.AcceptTCP()
	if err != nil {
		return err
	}
	assigned := false
	defer func() {
		if !assigned {
			// The caller reports both the handshake failure and any close failure.
			err = errors.Join(err, connection.Close())
		}
	}()
	if err = connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	line, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return fmt.Errorf("invalid ready PID %q: %w", line, err)
	}
	if pid <= 0 {
		return fmt.Errorf("invalid ready PID %d", pid)
	}
	f.mu.Lock()
	child := f.childLocked(pid)
	if child.handshake {
		f.mu.Unlock()
		return fmt.Errorf("duplicate handshake from child %d", pid)
	}
	child.connection, child.handshake = connection, true
	f.mu.Unlock()
	assigned = true
	return nil
}

func (f *daemonWaitFixture) ready(t *testing.T) {
	t.Helper()
	if err := f.acceptChild(); err != nil {
		t.Fatal(err)
	}
}

func (f *daemonWaitFixture) release(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.children {
		child := &f.children[index]
		if child.connection == nil {
			continue
		}
		if err := child.connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
			continue
		}
		child.connection = nil
	}
}

func (f *daemonWaitFixture) acceptPendingChildren(t *testing.T) bool {
	t.Helper()
	// Every registered child must cross its real producer handshake, including
	// a buggy duplicate. Explicit connection closure releases each child.
	for {
		f.mu.Lock()
		pending := false
		for _, child := range f.children {
			pending = pending || child.done != nil && !child.handshake
		}
		f.mu.Unlock()
		if !pending {
			return true
		}
		if err := f.acceptChild(); err != nil {
			t.Errorf("retain daemon fixture %s: child handshake: %v", f.root, err)
			return false
		}
	}
}

func (f *daemonWaitFixture) close(t *testing.T) {
	t.Helper()
	clean := f.acceptPendingChildren(t)
	f.release(t)
	if f.listener != nil {
		if err := f.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("retain daemon fixture %s: listener close: %v", f.root, err)
			clean = false
		}
	}
	f.mu.Lock()
	children := append([]daemonWaitChild(nil), f.children...)
	started := f.nativeStarted
	f.mu.Unlock()
	if started && f.registrations() == 0 {
		t.Errorf("retain daemon fixture %s: completion registration missing", f.root)
		clean = false
	}
	for _, child := range children {
		if child.done == nil {
			t.Errorf("retain daemon fixture %s: PID %d completion registration missing", f.root, child.pid)
			clean = false
		} else if !joinDaemonWait(t, child.done) {
			t.Errorf("retain daemon fixture %s: PID %d Wait unresolved", f.root, child.pid)
			clean = false
		}
		if child.connection != nil {
			t.Errorf("retain daemon fixture %s: PID %d connection closure unresolved", f.root, child.pid)
			clean = false
		}
	}
	if clean {
		if err := os.RemoveAll(f.root); err != nil {
			t.Error(err)
		}
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
	if f.registrations() != 1 || f.registrationRoot() != f.root || f.registrationPID() != f.pid() || f.done() == nil {
		t.Fatalf("wait registration differs: count=%d root=%t pid=%d want=%d", f.registrations(), f.registrationRoot() == f.root, f.registrationPID(), f.pid())
	}
	reference, err := lifetime.ObserveProcess(f.pid())
	if err != nil {
		t.Fatal(err)
	}
	present, err := lifetime.ProcessPresent(reference)
	if err != nil || !present {
		t.Fatalf("ready child is not alive: %v", err)
	}
	select {
	case <-f.done():
		t.Fatal("Wait completion closed while gated child is alive")
	default:
	}
	f.release(t)
	joinDaemonWait(t, f.done())
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
	if f.registrations() != 0 || f.done() != nil {
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
	joinDaemonWait(t, f.done())
}
