package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const ownerServerWaitChild = "DELEGATE_TEST_OWNER_SERVER_WAIT_CHILD"

func TestOwnerServerFiniteWaitChild(t *testing.T) {
	if os.Getenv(ownerServerWaitChild) != "1" {
		return
	}
	gate := os.NewFile(3, "finite-child-exit-gate")
	if gate == nil {
		t.Fatal("missing finite child gate")
	}
	defer func() {
		if err := gate.Close(); err != nil {
			t.Error(err)
		}
	}()
	// The finite Go test process timeout bounds this inherited blocking pipe.
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatal(err)
	}
	var data [1]byte
	if _, err := gate.Read(data[:]); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

type ownerServerOwnedChild struct {
	mu          sync.Mutex
	command     *exec.Cmd
	gateRead    *os.File
	gateWrite   *os.File
	output      *os.File
	outputWrite *os.File
	started     bool
	closed      bool
	done        chan struct{}
	waitErr     error
	waits       atomic.Int32
}

func newOwnerServerOwnedChild(t *testing.T, fixture *ownerServerFixture) *ownerServerOwnedChild {
	t.Helper()
	child := &ownerServerOwnedChild{done: make(chan struct{})}
	// This cleanup is registered before pipes or native Start and runs before
	// the enclosing server fixture can release its resident resources.
	t.Cleanup(func() { child.cleanup(t, fixture) })
	var err error
	child.gateRead, child.gateWrite, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child.command = exec.Command(executable, "-test.run=^TestOwnerServerFiniteWaitChild$", "-test.timeout=30s")
	child.command.Env = append(os.Environ(), ownerServerWaitChild+"=1")
	child.command.ExtraFiles = []*os.File{child.gateRead}
	child.output, child.outputWrite, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	child.command.Stdout = child.outputWrite
	if err = child.output.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return child
}

func (c *ownerServerOwnedChild) start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.started {
		return os.ErrClosed
	}
	if err := c.command.Start(); err != nil {
		return err
	}
	c.started = true
	go func() {
		c.waits.Add(1)
		c.waitErr = c.command.Wait()
		close(c.done)
	}()
	return nil
}

func (c *ownerServerOwnedChild) cleanup(t *testing.T, fixture *ownerServerFixture) {
	t.Helper()
	c.mu.Lock()
	c.closed = true
	started := c.started
	c.mu.Unlock()
	for _, file := range []*os.File{c.gateRead, c.gateWrite, c.outputWrite} {
		if file != nil {
			if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				t.Error(err)
			}
		}
	}
	if started {
		select {
		case <-c.done:
			if c.waitErr != nil {
				t.Error(c.waitErr)
			}
		case <-time.After(10 * time.Second):
			fixture.retain = true
			t.Errorf("unresolved finite child pid %d; retaining %s", c.command.Process.Pid, fixture.root)
			return
		}
	}
	if c.output != nil {
		if err := c.output.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error(err)
		}
	}
}

// This finite native child proves completion collection and exact reaping. It
// is not a Pueue daemon or trusted native resident-birth/placement proof.
func TestOwnerServerDrainRetainsLiveWaitUntilExactNativeReaping(t *testing.T) {
	host := &ownerServerTestHost{}
	f := newOwnerServerFixture(t, host)
	child := newOwnerServerOwnedChild(t, f)
	host.ensure = child.ensure
	f.start()
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, f.request(t, ownerEnsureOwn)); err != nil {
		t.Fatal(err)
	}
	if ready, err := bufio.NewReader(child.output).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("finite child readiness: %q %v", ready, err)
	}
	if reply := readServerReply(t, connection); reply.Code != ownerOK {
		t.Fatalf("finite child host call: %s", reply.Code)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	checkOwnerServerPausedLiveWait(t, f, child, ctx)
	if err := f.server.stopAcceptAndDrain(ctx); err != nil {
		t.Fatal(err)
	}
	// The child is deterministically blocked on its actual exit pipe. Draining
	// handlers must neither close that pipe nor join a still-live daemon Wait.
	select {
	case <-child.done:
		t.Fatal("Wait completion closed while child is alive")
	default:
	}
	f.server.waits.mu.Lock()
	active := len(f.server.waits.active)
	f.server.waits.mu.Unlock()
	if active != 1 {
		t.Fatalf("live child Wait handle count=%d", active)
	}
	if err := f.entry.validateResident(t.Context()); err != nil {
		t.Fatalf("drain closed resident: %v", err)
	}
	if err := child.gateWrite.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	child.verifyReaped(t, ctx, f.server.waits)
}

func (c *ownerServerOwnedChild) pid() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return 0
	}
	return c.command.Process.Pid
}

func (c *ownerServerOwnedChild) ensure(ctx context.Context, operation Operation, selected LaunchContext, guard DaemonStartGuard, waits DaemonWaitOwner) (Disposition, error) {
	if err := guard.GuardDaemonStart(ctx, operation.Intent().Inputs.Root, c.start); err != nil {
		return Disposition{}, err
	}
	if err := waits.RegisterDaemonWait(operation.Intent().Inputs.Root, c.pid(), c.done); err != nil {
		return Disposition{}, err
	}
	digest, err := selected.Digest()
	if err != nil {
		return Disposition{}, err
	}
	return Disposition{Kind: DispositionStarted, Context: LaunchContextReference{SHA256: digest}}, nil // finite child only, not daemon readiness proof
}

func (c *ownerServerOwnedChild) verifyReaped(t *testing.T, ctx context.Context, waits *daemonWaitOwner) {
	t.Helper()
	if c.waitErr != nil || c.waits.Load() != 1 {
		t.Fatalf("sole Wait: count=%d err=%v", c.waits.Load(), c.waitErr)
	}
	var information unix.Siginfo
	if err := unix.Waitid(unix.P_PID, c.pid(), &information, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil); !errors.Is(err, unix.ECHILD) {
		t.Fatalf("Wait done did not prove exact child reaped: %v", err)
	}
	if err := waits.join(ctx); err != nil {
		t.Fatalf("completed Wait collection: %v", err)
	}
}

func checkOwnerServerPausedLiveWait(t *testing.T, f *ownerServerFixture, child *ownerServerOwnedChild, ctx context.Context) {
	t.Helper()
	original := f.server.waits
	if err := f.server.pauseOperationsAndDrain(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.done:
		t.Fatal("pause closed the live child Wait")
	default:
	}
	completed := make(chan struct{})
	close(completed)
	if err := original.RegisterDaemonWait(f.root, 1, completed); err != nil {
		t.Fatalf("pause closed Wait registration: %v", err)
	}
	// Mechanics only: durable Aborted verification belongs to the future private
	// lifecycle caller, not this component fixture or this resume method.
	if err := f.server.resumeOperationsAfterVerifiedAbort(ctx); err != nil {
		t.Fatal(err)
	}
	if f.server.waits != original {
		t.Fatal("resume replaced Wait collection")
	}
	original.mu.Lock()
	pid, present := original.active[child.done]
	original.mu.Unlock()
	if !present || pid != child.pid() {
		t.Fatal("pause/resume lost the exact live completion handle")
	}
}
