//go:build linux

package lifetime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	linuxOrphanHelperMode = "DELEGATE_LINUX_ORPHAN_HELPER"
	linuxOrphanPIDFile    = "DELEGATE_LINUX_ORPHAN_PID_FILE"
)

func TestLinuxProcessScopeTerminatesReparentedMarkedSession(t *testing.T) {
	if runLinuxOrphanHelperMode(t) {
		return
	}
	fixture := startLinuxOrphanFixture(t)
	assertLinuxOrphanRetainedAndTerminated(t, fixture)
}

func TestLinuxProcessScopeReapsAdoptedZombie(t *testing.T) {
	if runLinuxOrphanHelperMode(t) {
		return
	}
	pidFile := filepath.Join(t.TempDir(), "zombie.pid")
	ref := testProcessGroupRef(t)
	scope, err := Open(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err = scope.BindOwnerProcess(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := scope.Release(); releaseErr != nil {
			t.Errorf("scope Release: %v", releaseErr)
		}
	})

	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxProcessScopeReapsAdoptedZombie$")
	cmd.Env = append(os.Environ(), linuxOrphanHelperMode+"=zombie-root", linuxOrphanPIDFile+"="+pidFile)
	if err = scope.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rootRef, err := scope.ObserveStarted(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	childPID := waitForLinuxOrphanPID(t, pidFile)
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	waitForZombie(t, childPID)
	empty, err := scope.Empty([]LaunchRef{rootRef})
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		t.Fatal("scope remained populated after reaping its adopted zombie")
	}
	present, _, _, err := inspectLinuxProcessIdentity(childPID)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatalf("adopted zombie PID %d remained present", childPID)
	}
}

type linuxOrphanFixture struct {
	scope    *Scope
	rootRef  LaunchRef
	childRef LaunchRef
	childPID int
	rootCmd  *exec.Cmd
}

func startLinuxOrphanFixture(t *testing.T) linuxOrphanFixture {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	ref := testProcessGroupRef(t)
	scope, err := Open(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err = scope.BindOwnerProcess(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := scope.Release(); releaseErr != nil {
			t.Errorf("scope Release: %v", releaseErr)
		}
	})
	fixture := &linuxOrphanFixture{scope: scope}
	t.Cleanup(func() { cleanupLinuxOrphanFixture(t, *fixture) })

	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxProcessScopeTerminatesReparentedMarkedSession$")
	cmd.Env = append(os.Environ(), linuxOrphanHelperMode+"=root", linuxOrphanPIDFile+"="+pidFile)
	if err = scope.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	fixture.rootCmd = cmd
	rootRef, err := scope.ObserveStarted(cmd.Process.Pid)
	if err != nil {
		if waitErr := cmd.Wait(); waitErr != nil {
			t.Logf("failed root helper exited with: %v", waitErr)
		}
		t.Fatal(err)
	}
	fixture.rootRef = rootRef
	childPID := waitForLinuxOrphanPID(t, pidFile)
	childPresent, childToken, err := observeStartedProcessIdentity(childPID)
	if err != nil || !childPresent {
		t.Fatalf("observe orphan child PID %d: present=%t err=%v", childPID, childPresent, err)
	}
	childPGID, err := processGroupID(childPID)
	if err != nil {
		t.Fatal(err)
	}
	childRef, err := NewLaunchRef(childPID, childToken, childPGID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.childRef = childRef
	fixture.childPID = childPID
	return *fixture
}

func assertLinuxOrphanRetainedAndTerminated(t *testing.T, fixture linuxOrphanFixture) {
	t.Helper()
	if waitErr := fixture.rootCmd.Wait(); waitErr != nil {
		t.Fatalf("orphan root helper: %v", waitErr)
	}
	rootPresent, _, err := observeProcessIdentity(fixture.rootRef.PID())
	if err != nil {
		t.Fatal(err)
	}
	if rootPresent {
		t.Fatalf("root PID %d remained present after Wait", fixture.rootRef.PID())
	}
	empty, err := fixture.scope.Empty([]LaunchRef{fixture.rootRef})
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Fatal("marked reparented child was not retained by the Linux process scope")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err = fixture.scope.Terminate(ctx, []LaunchRef{fixture.rootRef}); err != nil {
		t.Fatal(err)
	}
	childAlive, _, err := observeProcessIdentity(fixture.childPID)
	if err != nil {
		t.Fatal(err)
	}
	if childAlive {
		t.Fatalf("marked orphan child PID %d survived Linux scope termination", fixture.childPID)
	}
}

func cleanupLinuxOrphanFixture(t *testing.T, fixture linuxOrphanFixture) {
	t.Helper()
	refs := make([]LaunchRef, 0, 2)
	if fixture.rootRef.PID() > 0 {
		refs = append(refs, fixture.rootRef)
	}
	if fixture.childRef.PID() > 0 {
		refs = append(refs, fixture.childRef)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if len(refs) > 0 {
		if err := fixture.scope.Terminate(cleanupCtx, refs); err != nil {
			t.Logf("cleanup orphan scope: %v", err)
			for _, ref := range refs {
				killVerifiedLinuxTestProcess(t, ref)
			}
		}
	} else if fixture.rootCmd != nil && fixture.rootCmd.Process != nil && fixture.rootCmd.ProcessState == nil {
		if err := fixture.scope.Terminate(cleanupCtx, nil); err != nil {
			t.Logf("cleanup root helper scope: %v", err)
		}
	}
	if fixture.rootCmd != nil && fixture.rootCmd.Process != nil && fixture.rootCmd.ProcessState == nil {
		if err := fixture.rootCmd.Wait(); err != nil {
			t.Logf("wait root helper during cleanup: %v", err)
		}
	}
}

func runLinuxOrphanHelperMode(t *testing.T) bool {
	t.Helper()
	switch os.Getenv(linuxOrphanHelperMode) {
	case "root":
		runLinuxOrphanRootHelper(t)
		return true
	case "child":
		for {
			time.Sleep(time.Hour)
		}
	case "zombie-root":
		runLinuxZombieRootHelper(t)
		return true
	case "zombie-child":
		return true
	default:
		return false
	}
}

func killVerifiedLinuxTestProcess(t *testing.T, ref LaunchRef) {
	t.Helper()
	present, token, err := observeStartedProcessIdentity(ref.PID())
	if err != nil {
		t.Logf("observe cleanup PID %d: %v", ref.PID(), err)
		return
	}
	if !present || token != ref.StartToken() {
		return
	}
	if err = rawProcessSignal(ref.PID(), syscall.SIGKILL); err != nil && !errors.Is(err, errProcessNoSuchEntity) {
		t.Logf("kill cleanup PID %d: %v", ref.PID(), err)
	}
}

func runLinuxZombieRootHelper(t *testing.T) {
	pidFile := os.Getenv(linuxOrphanPIDFile)
	if pidFile == "" {
		t.Fatal("zombie PID file is empty")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestLinuxProcessScopeReapsAdoptedZombie$")
	child.Env = append(os.Environ(), linuxOrphanHelperMode+"=zombie-child")
	if err := configureProcessGroupCommand(nil, child); err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := publishTestPID(pidFile, child.Process.Pid); err != nil {
		t.Fatal(err)
	}
}

func TestParseLinuxProcessScopeStat(t *testing.T) {
	fields := []string{"S", "41", "42", "43"}
	for len(fields) < 19 {
		fields = append(fields, "0")
	}
	fields = append(fields, "12345")
	record, err := parseLinuxProcessScopeStat([]byte("44 (worker (nested)) " + strings.Join(fields, " ")))
	if err != nil {
		t.Fatal(err)
	}
	if record.zombie || record.snapshot.pid != 44 || record.snapshot.parentPID != 41 || record.snapshot.pgid != 42 || record.snapshot.sessionID != 43 || record.snapshot.startToken != "linux:12345" {
		t.Fatalf("parsed Linux process scope = %#v", record)
	}
	fields[19] = "0"
	record, err = parseLinuxProcessScopeStat([]byte("44 (worker (nested)) " + strings.Join(fields, " ")))
	if err != nil {
		t.Fatalf("parse process from the kernel's first clock tick: %v", err)
	}
	if record.snapshot.startToken != "linux:0" {
		t.Fatalf("first-tick start token = %q, want linux:0", record.snapshot.startToken)
	}
}

func TestLinuxPidfdVerificationRejectsProcessReusedAfterHandleOpen(t *testing.T) {
	ref, err := NewLaunchRef(4242, "linux:old", 4242)
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	handles, err := openVerifiedLinuxProcessHandles(4242, []LaunchRef{ref}, linuxPidfdOperations{
		open: func(pid int) (int, error) {
			if pid != ref.PID() {
				t.Fatalf("opened PID %d, want %d", pid, ref.PID())
			}
			return 9, nil
		},
		inspect: func(pid int) (linuxProcessScopeStat, bool, error) {
			return linuxProcessScopeStat{snapshot: processScopeSnapshot{
				pid: pid, pgid: ref.PGID(), sessionID: ref.PGID(), startToken: "linux:reused",
			}}, true, nil
		},
		close: func(fd int) error {
			if fd != 9 {
				t.Fatalf("closed pidfd %d, want 9", fd)
			}
			closed++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 0 || closed != 1 {
		t.Fatalf("reused PID retained handles=%+v closed=%d", handles, closed)
	}
}

func runLinuxOrphanRootHelper(t *testing.T) {
	pidFile := os.Getenv(linuxOrphanPIDFile)
	if pidFile == "" {
		t.Fatal("orphan PID file is empty")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestLinuxProcessScopeTerminatesReparentedMarkedSession$")
	child.Env = append(os.Environ(), linuxOrphanHelperMode+"=child")
	if err := configureProcessGroupCommand(nil, child); err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := publishTestPID(pidFile, child.Process.Pid); err != nil {
		t.Fatal(err)
	}
}

func waitForLinuxOrphanPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil || pid <= 1 {
				t.Fatalf("parse orphan PID %q: %v", data, parseErr)
			}
			return pid
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("orphan child PID was not published")
	return 0
}

func TestProcessGroupObservesStartedZombieBeforeWait(t *testing.T) {
	scope, cmd := commandForProcessGroup(t, "exit 0")
	t.Cleanup(func() {
		if releaseErr := scope.Release(); releaseErr != nil {
			t.Errorf("scope Release: %v", releaseErr)
		}
	})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			if waitErr := cmd.Wait(); waitErr != nil {
				t.Errorf("cleanup Wait: %v", waitErr)
			}
		}
	})

	waitForZombie(t, cmd.Process.Pid)

	launchRef, err := scope.ObserveStarted(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("ObserveStarted zombie: %v", err)
	}
	if launchRef.PID() != cmd.Process.Pid || launchRef.StartToken() == "" || launchRef.PGID() != cmd.Process.Pid {
		t.Fatalf("ObserveStarted zombie returned %#v", launchRef)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	waited = true
}

func waitForZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			t.Fatalf("read child stat: %v", err)
		}
		closing := strings.LastIndex(string(data), ")")
		if closing >= 0 {
			fields := strings.Fields(string(data)[closing+1:])
			if len(fields) > 0 && fields[0] == "Z" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not become a zombie before Wait")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLinuxProcessAbsentError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "wrapped ESRCH", err: fmt.Errorf("inspect process: %w", unix.ESRCH), want: true},
		{name: "wrapped not-exist", err: fmt.Errorf("inspect process: %w", os.ErrNotExist), want: true},
		{name: "other error", err: fmt.Errorf("inspect process: %w", unix.EPERM), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isLinuxProcessAbsentError(test.err); got != test.want {
				t.Fatalf("isLinuxProcessAbsentError(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}
