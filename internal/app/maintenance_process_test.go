package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	testsupportcore "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"golang.org/x/sys/unix"
)

const (
	queueMaintenanceLaunchHelper         = "DELEGATE_TEST_QUEUE_MAINTENANCE_LAUNCH"
	queueMaintenanceParentReady          = "DELEGATE_TEST_QUEUE_MAINTENANCE_PARENT_READY"
	queueMaintenanceDescriptorMarkerName = ".queue-maintenance-descriptor-check"
	queueMaintenanceDescriptorResultName = ".queue-maintenance-descriptor-result"
	queueMaintenanceDescriptorMinimum    = 200
)

func TestMain(m *testing.M) {
	if exit, handled := runLifetimeLaunchGateMode(os.Args[1:], os.Stderr); handled {
		os.Exit(exit)
	}
	if len(os.Args) > 1 && os.Args[1] == "--queue-maintenance" {
		if err := recordQueueMaintenanceDescriptorInheritance(os.Args[1:]); err != nil {
			os.Exit(2)
		}
		os.Exit(RunRunner(os.Args[1:], io.Discard, io.Discard, Dependencies{}))
	}
	os.Exit(m.Run())
}

func recordQueueMaintenanceDescriptorInheritance(args []string) error {
	root := ""
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "--root" {
			root = args[index+1]
			break
		}
	}
	if root == "" {
		return nil
	}
	markerPath := filepath.Join(root, queueMaintenanceDescriptorMarkerName)
	data, err := os.ReadFile(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	fd, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || fd < 3 {
		return errors.Join(pueue.ErrConfiguration, err)
	}
	state := "inherited\n"
	if _, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); errors.Is(err, unix.EBADF) {
		state = "closed\n"
	} else if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, queueMaintenanceDescriptorResultName), []byte(state), 0o600)
}

type blockedQueueMaintenanceFixture struct {
	root        string
	base        string
	releasePath string
	released    bool
}

func newBlockedQueueMaintenanceFixture(t *testing.T) *blockedQueueMaintenanceFixture {
	t.Helper()
	store, _, _, job := newRecordedTerminalCleanupFixture(t)
	if err := pueue.RecordPrivateTerminalEvent(t.Context(), store.Root, job.ID, "0"); err != nil {
		t.Fatal(err)
	}
	if err := pueue.RequestPrivateCandidateQueueMaintenance(t.Context(), store.Root); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(store.Root, ".supervisor")
	if err := os.WriteFile(filepath.Join(base, "block-maintenance-remove"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := &blockedQueueMaintenanceFixture{
		root:        store.Root,
		base:        base,
		releasePath: filepath.Join(base, "release-maintenance-remove"),
	}
	t.Cleanup(func() { fixture.releaseForCleanup(t) })
	return fixture
}

func (fixture *blockedQueueMaintenanceFixture) waitBlocked(t *testing.T) {
	t.Helper()
	testsupport.WaitForTestPath(t, filepath.Join(fixture.base, "maintenance-remove-blocked"))
}

func (fixture *blockedQueueMaintenanceFixture) assertStillBlocked(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(fixture.base, "terminal-event-removals")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("maintenance removed row before test release: %v", err)
	}
}

func (fixture *blockedQueueMaintenanceFixture) release(t *testing.T) {
	t.Helper()
	if fixture.released {
		return
	}
	if err := os.WriteFile(fixture.releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.released = true
}

func (fixture *blockedQueueMaintenanceFixture) releaseForCleanup(t *testing.T) {
	t.Helper()
	if fixture.released {
		return
	}
	if err := os.WriteFile(fixture.releasePath, nil, 0o600); err != nil {
		t.Errorf("release maintenance helper: %v", err)
		return
	}
	fixture.released = true
}

func (fixture *blockedQueueMaintenanceFixture) assertCompleted(t *testing.T) {
	t.Helper()
	waitForTestFileContent(t, filepath.Join(fixture.base, "terminal-event-removals"), "7\n")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := pueue.WaitForPrivateQueueMaintenanceIdle(ctx, fixture.root); err != nil {
		t.Fatal(err)
	}
	pending, err := pueue.PrivateQueueMaintenanceRetryNeededContext(ctx, fixture.root)
	if err != nil || pending {
		t.Fatalf("maintenance after owner handoff pending=%t err=%v", pending, err)
	}
}

func TestQueueMaintenanceOwnerSurvivesInitiatingProcessExit(t *testing.T) {
	fixture := newBlockedQueueMaintenanceFixture(t)
	executable, err := filepath.EvalSymlinks(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(executable, "-test.run", "^TestQueueMaintenanceLaunchParent$", "-test.count=1")
	parent.Env = append(os.Environ(), queueMaintenanceLaunchHelper+"="+fixture.root)
	if output, runErr := parent.CombinedOutput(); runErr != nil {
		t.Fatalf("maintenance launch parent: %v: %s", runErr, output)
	}
	fixture.waitBlocked(t)
	fixture.assertStillBlocked(t)
	fixture.release(t)
	fixture.assertCompleted(t)
}

func TestQueueMaintenanceLaunchParent(t *testing.T) {
	root := os.Getenv(queueMaintenanceLaunchHelper)
	if root == "" {
		t.Skip("subprocess helper")
	}
	executable, err := filepath.EvalSymlinks(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = supervisor.LaunchQueueMaintenanceOwner(context.Background(), executable, root); err != nil {
		t.Fatal(err)
	}
	if readyPath := os.Getenv(queueMaintenanceParentReady); readyPath != "" {
		if err = os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err = io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
	}
}

func TestQueueMaintenanceOwnerSurvivesCallerProcessGroupInterruption(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("independent Unix sessions require Linux or Darwin")
	}
	fixture := newBlockedQueueMaintenanceFixture(t)
	caller := startScopedQueueMaintenanceCaller(t, fixture.root, fixture.base)
	testsupport.WaitForTestPath(t, caller.readyPath)
	fixture.waitBlocked(t)
	caller.interrupt(t)
	fixture.assertStillBlocked(t)
	fixture.release(t)
	fixture.assertCompleted(t)
}

type scopedQueueMaintenanceCaller struct {
	command   *exec.Cmd
	stdin     io.WriteCloser
	output    bytes.Buffer
	readyPath string
	started   bool
	waited    bool
}

func startScopedQueueMaintenanceCaller(t *testing.T, root, base string) *scopedQueueMaintenanceCaller {
	t.Helper()
	executable := queueMaintenanceTestExecutable(t)
	caller := &scopedQueueMaintenanceCaller{readyPath: filepath.Join(base, "maintenance-parent-ready")}
	t.Cleanup(func() { caller.cleanup(t) })
	caller.command = exec.Command(executable, "-test.run", "^TestQueueMaintenanceLaunchParent$", "-test.count=1")
	caller.command.Env = append(
		os.Environ(),
		queueMaintenanceLaunchHelper+"="+root,
		queueMaintenanceParentReady+"="+caller.readyPath,
	)
	stdin, err := caller.command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	caller.stdin = stdin
	caller.command.Stdout = &caller.output
	caller.command.Stderr = &caller.output
	if err = lifetime.ConfigureIndependentSession(caller.command); err != nil {
		t.Fatal(err)
	}
	if err = caller.command.Start(); err != nil {
		t.Fatal(err)
	}
	caller.started = true
	return caller
}

func (caller *scopedQueueMaintenanceCaller) interrupt(t *testing.T) {
	t.Helper()
	interruptQueueMaintenanceCallerGroup(t, caller.command.Process.Pid)
	caller.closeStdin(t)
	if waitErr := caller.wait(); waitErr == nil {
		t.Fatalf("interrupted caller exited successfully: %s", caller.output.Bytes())
	}
}

func interruptQueueMaintenanceCallerGroup(t *testing.T, pid int) {
	t.Helper()
	signal := exec.Command("/bin/sh", "-c", `kill -TERM -"$1"`, "sh", strconv.Itoa(pid))
	if output, err := signal.CombinedOutput(); err != nil {
		t.Fatalf("interrupt caller process group: %v: %s", err, output)
	}
}

func (caller *scopedQueueMaintenanceCaller) closeStdin(t *testing.T) {
	t.Helper()
	if caller.stdin == nil {
		return
	}
	if err := caller.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Error(err)
	}
	caller.stdin = nil
}

func (caller *scopedQueueMaintenanceCaller) wait() error {
	if !caller.started || caller.waited {
		return nil
	}
	err := caller.command.Wait()
	caller.waited = true
	return err
}

func (caller *scopedQueueMaintenanceCaller) cleanup(t *testing.T) {
	t.Helper()
	caller.closeStdin(t)
	if caller.started && !caller.waited {
		if waitErr := caller.wait(); waitErr != nil {
			t.Logf("cleanup wait for caller helper: %v", waitErr)
		}
	}
}

func TestQueueMaintenanceOwnerDoesNotInheritUnrelatedDescriptor(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("descriptor inheritance test requires Unix fcntl semantics")
	}
	root := testsupportcore.CanonicalTempDir(t)
	descriptor := openQueueMaintenanceParentDescriptor(t)
	writeQueueMaintenanceDescriptorMarker(t, root, descriptor)
	executable := queueMaintenanceTestExecutable(t)
	done, err := supervisor.StartQueueMaintenanceOwner(t.Context(), executable, root)
	if err != nil {
		t.Fatal(err)
	}
	waitForFailedQueueMaintenanceOwner(t, done)
	assertQueueMaintenanceDescriptorClosedInChild(t, root, descriptor)
}

func openQueueMaintenanceParentDescriptor(t *testing.T) *os.File {
	t.Helper()
	source, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	fd, duplicateErr := unix.FcntlInt(source.Fd(), unix.F_DUPFD_CLOEXEC, queueMaintenanceDescriptorMinimum)
	if closeErr := source.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if duplicateErr != nil {
		t.Fatal(duplicateErr)
	}
	descriptor := os.NewFile(uintptr(fd), "queue-maintenance-unrelated-parent-descriptor")
	if descriptor == nil {
		if closeErr := unix.Close(fd); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("duplicate descriptor could not be wrapped")
	}
	t.Cleanup(func() {
		if closeErr := descriptor.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	flags, err := unix.FcntlInt(descriptor.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("unrelated parent descriptor is not guarded by FD_CLOEXEC")
	}
	return descriptor
}

func writeQueueMaintenanceDescriptorMarker(t *testing.T, root string, descriptor *os.File) {
	t.Helper()
	err := os.WriteFile(
		filepath.Join(root, queueMaintenanceDescriptorMarkerName),
		[]byte(strconv.FormatUint(uint64(descriptor.Fd()), 10)+"\n"),
		0o600,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func assertQueueMaintenanceDescriptorClosedInChild(t *testing.T, root string, descriptor *os.File) {
	t.Helper()
	result, err := os.ReadFile(filepath.Join(root, queueMaintenanceDescriptorResultName))
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != "closed\n" {
		t.Fatalf("unrelated parent descriptor state in maintenance helper = %q", result)
	}
	if _, err = unix.FcntlInt(descriptor.Fd(), unix.F_GETFD, 0); err != nil {
		t.Fatalf("maintenance launch closed the parent's descriptor: %v", err)
	}
}

func queueMaintenanceTestExecutable(t *testing.T) string {
	t.Helper()
	executable, err := filepath.EvalSymlinks(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	return executable
}

func waitForFailedQueueMaintenanceOwner(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case waitErr := <-done:
		if waitErr == nil {
			t.Fatal("maintenance helper without a supervisor binding exited successfully")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance helper was not reaped")
	}
}

func TestQueueMaintenanceOwnerIsReapedWhileCallerLives(t *testing.T) {
	executable := queueMaintenanceTestExecutable(t)
	var err error
	done, err := supervisor.StartQueueMaintenanceOwner(t.Context(), executable, filepath.Join(t.TempDir(), "missing-state"))
	if err != nil {
		t.Fatal(err)
	}
	waitForFailedQueueMaintenanceOwner(t, done)
}
