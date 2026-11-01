//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"golang.org/x/sys/unix"
)

func teamsIsolationSubreaper(t *testing.T) int32 {
	t.Helper()
	var value int32
	// PR_GET_CHILD_SUBREAPER writes one native int; this synchronous syscall
	// needs its address and does not retain the pointer.
	_, _, errno := unix.Syscall6(unix.SYS_PRCTL, unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&value)), 0, 0, 0, 0)
	if errno != 0 {
		t.Fatalf("PR_GET_CHILD_SUBREAPER: %v", errno)
	}
	return value
}

func TestTeamsProjectIsolationPreservesCallerSubreaper(t *testing.T) {
	if os.Getenv("DELEGATE_TEAMS_SUBREAPER_CHECK") != "1" {
		runFreshTeamsSubreaperCheck(t)
		return
	}
	before := teamsIsolationSubreaper(t)
	if before != 0 {
		t.Fatalf("fresh process inherited unexpected subreaper state: %d", before)
	}
	resolveIsolatedTeamsDirectory(t)
	if after := teamsIsolationSubreaper(t); after != before {
		t.Fatalf("in-process project resolution changed caller subreaper: before=%d after=%d", before, after)
	}
}

func runFreshTeamsSubreaperCheck(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	command := exec.Command(executable, "-test.run=^TestTeamsProjectIsolationPreservesCallerSubreaper$")
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(), "DELEGATE_TEAMS_SUBREAPER_CHECK=1")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if runErr := (teamsProjectRunner{}).Run(ctx, command); runErr != nil {
		t.Fatalf("fresh in-process subreaper check failed: %v; stdout=%q stderr=%q", runErr, stdout.String(), stderr.String())
	}
}

type teamsIsolationIdentity struct {
	ref lifetime.ProcessRef
	fd  int
}

func teamsIsolationPIDFDs(t *testing.T, process *teamsIsolationProcess) []teamsIsolationIdentity {
	t.Helper()
	var ids teamsIsolationPIDs
	select {
	case ids = <-process.ids:
	case <-process.done:
		t.Fatalf("probe ended before PID acknowledgement: %v", process.err)
	case <-time.After(10 * time.Second):
		t.Fatal("probe never acknowledged parent and child PIDs")
	}
	var identities []teamsIsolationIdentity
	for _, pid := range []int{ids.Parent, ids.Child} {
		ref, err := lifetime.ObserveStartedProcess(pid)
		if err != nil {
			t.Fatalf("observe acknowledged PID %d birth identity: %v", pid, err)
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			t.Fatalf("open stable identity for acknowledged PID %d: %v", pid, err)
		}
		t.Cleanup(func() {
			if closeErr := unix.Close(fd); closeErr != nil {
				t.Error(closeErr)
			}
		})
		again, err := lifetime.ObserveStartedProcess(pid)
		if err != nil || again != ref || teamsIsolationFDInfoPID(t, fd) != pid {
			t.Fatalf("acknowledged PID %d changed identity while opening pidfd: before=%+v after=%+v err=%v", pid, ref, again, err)
		}
		identities = append(identities, teamsIsolationIdentity{ref: ref, fd: fd})
	}
	return identities
}

func teamsIsolationFDInfoPID(t *testing.T, fd int) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc/self/fdinfo", strconv.Itoa(fd)))
	if err != nil {
		t.Fatal(err)
	}
	pid, found := 0, false
	for _, line := range strings.Split(string(data), "\n") {
		field, value, separated := strings.Cut(line, ":")
		if !separated || field != "Pid" {
			continue
		}
		if found {
			t.Fatal("duplicate Pid field in retained pidfd fdinfo")
		}
		pid, err = strconv.Atoi(strings.TrimSpace(value))
		if err != nil || pid < -1 {
			t.Fatalf("invalid pidfd Pid field %q: %v", value, err)
		}
		found = true
	}
	if !found {
		t.Fatal("retained pidfd fdinfo is missing Pid")
	}
	return pid
}

func requireTeamsIsolationReaped(t *testing.T, identities []teamsIsolationIdentity) {
	t.Helper()
	for _, identity := range identities {
		poll := []unix.PollFd{{Fd: int32(identity.fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(poll, 0); err != nil {
			t.Fatal(err)
		}
		// Linux 6.8 pidfd_show_fdinfo reports Pid=-1 only after the pinned
		// identity has no task. Its pidfd_poll does not implement POLLHUP.
		// https://raw.githubusercontent.com/torvalds/linux/v6.8/kernel/fork.c
		pid := teamsIsolationFDInfoPID(t, identity.fd)
		if pid != -1 || poll[0].Revents&unix.POLLIN == 0 {
			live, liveErr := lifetime.ProcessPresent(identity.ref)
			current, identityErr := lifetime.ObserveStartedProcess(identity.ref.PID())
			t.Errorf("runner returned before acknowledged identity was reaped: pid=%d birth=%q pidfd=%d fdinfo_pid=%d events=%#x live=%t live_err=%v same_birth_observable=%t identity_err=%v",
				identity.ref.PID(), identity.ref.StartToken(), identity.fd, pid, poll[0].Revents, live, liveErr, identityErr == nil && current == identity.ref, identityErr)
		}
	}
}

func TestTeamsProjectIsolationCancellationJoinsProbeAndChild(t *testing.T) {
	process := startTeamsIsolationProcess(t, true, true)
	fds := teamsIsolationPIDFDs(t, process)
	process.stop()
	if err := waitTeamsIsolationDone(t, process); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost its cause: %v", err)
	}
	requireTeamsIsolationReaped(t, fds)
}

func TestTeamsProjectIsolationNaturalFinishJoinsChildAndLeavesUnrelatedProcessResponsive(t *testing.T) {
	unrelated := startTeamsIsolationProcess(t, false, false)
	process := startTeamsIsolationProcess(t, true, true)
	fds := teamsIsolationPIDFDs(t, process)
	if err := process.releaseInput(); err != nil {
		t.Fatal(err)
	}
	if err := waitTeamsIsolationDone(t, process); err != nil {
		t.Fatalf("natural probe completion failed: %v", err)
	}
	requireTeamsIsolationReaped(t, fds)
	requireTeamsIsolationResponsive(t, unrelated)
}
