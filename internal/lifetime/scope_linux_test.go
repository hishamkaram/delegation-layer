//go:build linux

package lifetime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	linuxCgroupDescendantHelperMode = "DELEGATE_LINUX_CGROUP_DESCENDANT_HELPER"
	linuxCgroupDescendantPIDFile    = "DELEGATE_LINUX_CGROUP_DESCENDANT_PID_FILE"
	linuxCgroupDescendantWorkDir    = "DELEGATE_LINUX_CGROUP_DESCENDANT_WORK_DIR"
)

func TestParseCgroupEvents(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
		err   bool
	}{
		{name: "empty", input: "populated 0\nfrozen 0\n", want: true},
		{name: "populated", input: "populated 1\nfrozen 0\n", want: false},
		{name: "missing populated", input: "frozen 0\n", err: true},
		{name: "duplicate populated", input: "populated 0\npopulated 1\n", err: true},
		{name: "malformed line", input: "populated\n", err: true},
		{name: "malformed value", input: "populated yes\n", err: true},
		{name: "out of range", input: "populated 2\n", err: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseCgroupEvents([]byte(test.input))
			if test.err {
				if err == nil {
					t.Fatal("parseCgroupEvents returned nil error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCgroupEvents error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseCgroupEvents = %t, want %t", got, test.want)
			}
		})
	}
}

func TestResolveCgroupLocationFromProcFiles(t *testing.T) {
	cgroupData := []byte("12:name=legacy:/legacy\n0::/user.slice/user-1000.slice/session.scope\n")
	mountInfo := []byte("36 25 0:32 /user.slice /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw\n")
	location, err := resolveCgroupLocation(cgroupData, mountInfo)
	if err != nil {
		t.Fatal(err)
	}
	if location.mountRoot != "/user.slice" {
		t.Fatalf("mount root = %q, want /user.slice", location.mountRoot)
	}
	if location.mountPath != "/sys/fs/cgroup" {
		t.Fatalf("mount path = %q, want /sys/fs/cgroup", location.mountPath)
	}
	wantCurrent := "/sys/fs/cgroup/user-1000.slice/session.scope"
	if location.currentPath != wantCurrent {
		t.Fatalf("current path = %q, want %q", location.currentPath, wantCurrent)
	}
}

func TestParseMountInfoUnescapesCgroupPath(t *testing.T) {
	mounts, err := parseMountInfo([]byte("36 25 0:32 / /sys/fs/cgroup\\040delegated rw - cgroup2 cgroup rw\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].mountPath != "/sys/fs/cgroup delegated" {
		t.Fatalf("mounts = %#v", mounts)
	}
}

func TestParseCgroupV2PathRejectsMalformedInput(t *testing.T) {
	for _, input := range []string{
		"1:name=systemd:/system.slice\n",
		"0::relative\n",
		"0::/one\n0::/two\n",
	} {
		if _, err := parseCgroupV2Path([]byte(input)); err == nil {
			t.Fatalf("parseCgroupV2Path(%q) returned nil error", input)
		}
	}
}

func TestCgroupEmptyDoesNotTreatReadErrorsAsEmpty(t *testing.T) {
	empty, err := cgroupEmpty("/definitely/missing/delegation-layer-cgroup")
	if empty || err == nil {
		t.Fatalf("cgroupEmpty = (%t, %v), want false/non-nil", empty, err)
	}
}

func TestOpenRejectsMalformedCgroupReferences(t *testing.T) {
	for _, identifier := range []string{
		"cgroup2:not-base64:1:2",
		"cgroup2:Ly90bXAvLi4=:1:2",
		"cgroup2:Ly90bXA=:not-a-number:2",
	} {
		ref, err := NewRef(KindCgroupV2, AssuranceExact, identifier)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ref); !errors.Is(err, ErrInvalidRef) {
			t.Fatalf("Open(%q) error = %v, want ErrInvalidRef", identifier, err)
		}
	}
}

func TestLinuxCgroupOpenChecksIdentityAndConfigureConflicts(t *testing.T) {
	plan, err := Prepare("open-root", "task", "open-task")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := CreatePrepared(plan)
	if err != nil {
		t.Fatal(err)
	}
	ref := scope.Reference()
	if ref.Kind() != KindCgroupV2 {
		if releaseErr := scope.Release(); releaseErr != nil {
			t.Logf("fallback scope cleanup: %v", releaseErr)
		}
		t.Skipf("cgroup v2 delegation unavailable; selected %s", ref.Kind())
	}
	t.Cleanup(func() { cleanupLinuxScopeAfterDestroy(t, scope, "created cgroup") })

	reopened, err := Open(ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupLinuxScopeAfterDestroy(t, reopened, "reopened cgroup") })

	path, dev, ino, err := decodeCgroupIdentifier(ref.Identifier())
	if err != nil {
		t.Fatal(err)
	}
	wrongIdentity, err := NewRef(KindCgroupV2, AssuranceExact, encodeCgroupIdentifier(path, dev, ino+1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(wrongIdentity); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("Open wrong cgroup identity error = %v, want ErrInvalidRef", err)
	}

	cmd := exec.Command("sleep", "1")
	cmd.Dir = t.TempDir()
	attr := &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: 1234}
	cmd.SysProcAttr = attr
	if err := reopened.Configure(cmd); !errors.Is(err, ErrConflictingSysProc) {
		t.Fatalf("cgroup Configure error = %v, want ErrConflictingSysProc", err)
	}
	if cmd.SysProcAttr != attr || attr.CgroupFD != 1234 {
		t.Fatalf("cgroup Configure changed conflicting SysProcAttr: %#v", cmd.SysProcAttr)
	}

	if err := scope.DestroyEmpty(); err != nil {
		t.Fatal(err)
	}
	if err := scope.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxCgroupOpenReportsAbsentRecordedScope(t *testing.T) {
	plan, err := Prepare("absent-root", "task", "absent-task")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := CreatePrepared(plan)
	if err != nil {
		t.Fatal(err)
	}
	ref := scope.Reference()
	if ref.Kind() != KindCgroupV2 {
		if releaseErr := scope.Release(); releaseErr != nil {
			t.Logf("fallback scope cleanup: %v", releaseErr)
		}
		t.Skipf("cgroup v2 delegation unavailable; selected %s", ref.Kind())
	}
	t.Cleanup(func() { cleanupLinuxScopeAfterDestroy(t, scope, "removed cgroup") })

	path, _, _, err := decodeCgroupIdentifier(ref.Identifier())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := scope.DestroyEmpty(); err != nil {
		t.Fatalf("DestroyEmpty after external removal: %v", err)
	}
	if err := scope.Release(); err != nil {
		t.Fatalf("Release after external removal: %v", err)
	}
	if err := scope.DestroyEmpty(); err != nil {
		t.Fatalf("repeated DestroyEmpty after external removal: %v", err)
	}
	if _, err := Open(ref); !errors.Is(err, ErrScopeAbsent) {
		t.Fatalf("Open removed cgroup error = %v, want ErrScopeAbsent", err)
	}
}

func TestLinuxProcessStatParser(t *testing.T) {
	suffix := append([]string{"S"}, make([]string, 19)...)
	for index := range suffix {
		suffix[index] = "0"
	}
	suffix[0] = "S"
	suffix[19] = "42"
	present, token, err := parseLinuxProcessStat([]byte("123 (worker (nested)) " + strings.Join(suffix, " ")))
	if err != nil || !present || token != "linux:42" {
		t.Fatalf("parseLinuxProcessStat = (%t, %q, %v)", present, token, err)
	}
	zombieSuffix := append([]string{"Z"}, make([]string, 19)...)
	for index := range zombieSuffix {
		zombieSuffix[index] = "0"
	}
	zombieSuffix[0] = "Z"
	zombieSuffix[19] = "42"
	present, token, err = parseLinuxProcessStat([]byte("123 (worker) " + strings.Join(zombieSuffix, " ")))
	if err != nil || present || token != "" {
		t.Fatalf("zombie parse = (%t, %q, %v)", present, token, err)
	}
}

func TestLinuxNativeCgroupContainsSetsidDescendant(t *testing.T) {
	plan, err := Prepare("native-root", "task", "setsid-descendant")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind() != KindCgroupV2 {
		t.Skipf("cgroup v2 delegation unavailable; selected %s", plan.Kind())
	}
	exerciseLinuxNativeCgroupContainment(t, plan, nil)
}

func TestLinuxNativeCgroupDescendantHelper(t *testing.T) {
	if os.Getenv(linuxCgroupDescendantHelperMode) != "1" {
		return
	}
	pidFile := os.Getenv(linuxCgroupDescendantPIDFile)
	workDir := os.Getenv(linuxCgroupDescendantWorkDir)
	if pidFile == "" || workDir == "" {
		t.Fatal("cgroup descendant helper inputs are missing")
	}
	child := exec.Command("sleep", "5")
	child.Dir = workDir
	if err := ConfigureIndependentSession(child); err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := publishTestPID(pidFile, child.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		return
	}
}

func exerciseLinuxNativeCgroupContainment(t *testing.T, plan Plan, receipt *nativeBackendProofReceipt) nativeBackendObservation {
	t.Helper()
	fixture := startNativeCgroupCommand(t, plan, receipt)
	scope := fixture.scope
	cmd := fixture.cmd
	childPIDPath := fixture.childPIDPath
	launchRef, err := scope.ObserveStarted(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("observe cgroup launch: %v", err)
	}
	fixture.launchRef = launchRef
	fixture.launchObserved = true
	childPID := waitForLinuxOrphanPID(t, childPIDPath)
	childRef, err := ObserveStartedProcess(childPID)
	if err != nil {
		t.Fatalf("observe setsid descendant PID %d: %v", childPID, err)
	}
	childPresent, err := ProcessPresent(childRef)
	if err != nil || !childPresent {
		t.Fatalf("setsid descendant before termination = present %t, err %v", childPresent, err)
	}
	if destroyErr := scope.DestroyEmpty(); !errors.Is(destroyErr, ErrNotEmpty) {
		t.Fatalf("DestroyEmpty on populated cgroup = %v, want ErrNotEmpty", destroyErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if terminateErr := scope.Terminate(ctx, []LaunchRef{launchRef}); terminateErr != nil {
		t.Fatalf("Terminate: %v", terminateErr)
	}
	waitErr := cmd.Wait()
	fixture.waited = true
	if waitErr == nil {
		t.Fatal("terminated cgroup command returned nil Wait error")
	}
	empty, err := scope.Empty([]LaunchRef{launchRef})
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		t.Fatal("cgroup remained populated after cgroup.kill")
	}
	childPresent, err = ProcessPresent(childRef)
	if err != nil {
		t.Fatal(err)
	}
	if childPresent {
		t.Fatalf("setsid descendant PID %d survived cgroup termination", childPID)
	}
	if err := scope.DestroyEmpty(); err != nil {
		recordNativeProofDestroyFailure(receipt, scope)
		t.Fatalf("DestroyEmpty after cgroup.kill: %v", err)
	}
	fixture.destroyed = true
	if err := scope.Release(); err != nil {
		recordNativeProofReleaseFailure(receipt, scope)
		t.Fatalf("Release after cgroup.kill: %v", err)
	}
	fixture.released = true
	recordNativeProofReleased(receipt)
	if err := scope.DestroyEmpty(); err != nil {
		t.Fatalf("repeated DestroyEmpty after cgroup.kill: %v", err)
	}
	return nativeBackendObservation{
		primitive:       "cgroup-v2 cgroup.kill with setsid descendant",
		observed:        "setsid descendant terminated; command Wait consumed once; scope empty and destroyed",
		scopeIdentifier: scope.Reference().Identifier(),
	}
}

type nativeCgroupFixture struct {
	scope          *Scope
	cmd            *exec.Cmd
	childPIDPath   string
	launchRef      LaunchRef
	launchObserved bool
	waited         bool
	destroyed      bool
	released       bool
	receipt        *nativeBackendProofReceipt
}

func startNativeCgroupCommand(t *testing.T, plan Plan, receipt *nativeBackendProofReceipt) *nativeCgroupFixture {
	t.Helper()
	scope, err := CreatePrepared(plan)
	if err != nil {
		t.Fatal(err)
	}
	invocationDir := t.TempDir()
	childPIDPath := filepath.Join(invocationDir, "setsid-descendant.pid")
	cmd := exec.Command(os.Args[0], "-test.run", "^TestLinuxNativeCgroupDescendantHelper$")
	cmd.Dir = invocationDir
	cmd.Env = append(os.Environ(),
		linuxCgroupDescendantHelperMode+"=1",
		linuxCgroupDescendantPIDFile+"="+childPIDPath,
		linuxCgroupDescendantWorkDir+"="+invocationDir,
	)
	fixture := &nativeCgroupFixture{scope: scope, cmd: cmd, childPIDPath: childPIDPath, receipt: receipt}
	t.Cleanup(func() { cleanupNativeCgroupFixture(t, fixture) })
	if configureErr := scope.Configure(cmd); configureErr != nil {
		t.Fatal(configureErr)
	}
	if startErr := cmd.Start(); startErr != nil {
		t.Fatal(startErr)
	}
	return fixture
}

func cleanupNativeCgroupFixture(t *testing.T, fixture *nativeCgroupFixture) {
	t.Helper()
	if fixture.cmd.Process != nil && !fixture.waited {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		refs := []LaunchRef(nil)
		if fixture.launchObserved {
			refs = []LaunchRef{fixture.launchRef}
		}
		if err := fixture.scope.Terminate(ctx, refs); err != nil {
			t.Errorf("cleanup cgroup Terminate: %v", err)
		}
		cancel()
		if err := fixture.cmd.Wait(); err != nil {
			t.Logf("cleanup cgroup command exited with: %v", err)
		}
		fixture.waited = true
	}
	if !fixture.destroyed {
		if err := fixture.scope.DestroyEmpty(); err != nil {
			if fixture.receipt != nil {
				recordNativeProofDestroyFailure(fixture.receipt, fixture.scope)
			}
			t.Errorf("cleanup cgroup DestroyEmpty: %v", err)
			return
		}
		fixture.destroyed = true
	}
	if !fixture.released {
		if err := fixture.scope.Release(); err != nil {
			if fixture.receipt != nil {
				recordNativeProofReleaseFailure(fixture.receipt, fixture.scope)
			}
			t.Errorf("cleanup cgroup Release: %v", err)
			return
		}
		fixture.released = true
		recordNativeProofReleased(fixture.receipt)
	}
}

func cleanupLinuxScopeAfterDestroy(t *testing.T, scope *Scope, label string) {
	t.Helper()
	if err := scope.DestroyEmpty(); err != nil {
		t.Errorf("%s DestroyEmpty: %v", label, err)
		return
	}
	if err := scope.Release(); err != nil {
		t.Errorf("%s Release: %v", label, err)
	}
}
