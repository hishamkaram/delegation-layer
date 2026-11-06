//go:build linux

package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// WNOWAIT observes child status without stealing the sole cmd.Wait's reaping.
func requireDaemonWaitECHILD(t *testing.T, pid int) {
	t.Helper()
	var status unix.Siginfo
	err := unix.Waitid(unix.P_PID, pid, &status, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
	if !errors.Is(err, unix.ECHILD) {
		t.Fatalf("PID %d remains waitable after completion: %v", pid, err)
	}
}

func TestDaemonWaitDoneMeansChildAlreadyReaped(t *testing.T) {
	f := exerciseDaemonWaitRegistration(t, nil, nil)
	requireDaemonWaitECHILD(t, f.pid())
}

func TestDaemonWaitRunsBeforeGuardReturns(t *testing.T) {
	f := newDaemonWaitFixture(t)
	gate := make(chan struct{})
	var release sync.Once
	finished := make(chan error, 1)
	launched, joined := false, false
	var result error
	join := func() {
		if launched && !joined {
			result = <-finished
			joined = true
		}
	}
	// Release the guard and join the invoking call before fixture/child cleanup.
	t.Cleanup(func() { release.Do(func() { close(gate) }); join() })
	postStart := errors.New("guard post-start failure")
	options := f.options(nil)
	options.DaemonStartGuard = daemonStartGuardFunc(func(_ context.Context, _ string, start func() error) error {
		if err := start(); err != nil {
			return err
		}
		<-gate
		return postStart
	})
	launched = true
	go func() { finished <- f.start(t.Context(), options) }()
	f.ready(t)
	if f.registrations() != 0 {
		t.Fatal("wait registered before guard returned")
	}
	pidPath := filepath.Join(f.root, "pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(f.pid())), 0o600); err != nil {
		t.Fatal(err)
	}
	f.release(t)
	// Observe actual disappearance, then the nonreaping ECHILD oracle. No
	// scheduling delay or zombie-as-absent observation establishes this result.
	waitDaemonGuardChildReaped(t, pidPath)
	requireDaemonWaitECHILD(t, f.pid())
	release.Do(func() { close(gate) })
	join()
	if !errors.Is(result, postStart) || f.registrations() != 1 || f.registrationPID() != f.pid() || f.registrationRoot() != f.root {
		t.Fatalf("post-guard registration lost: %v count=%d", result, f.registrations())
	}
	joinDaemonWait(t, f.done())
}
