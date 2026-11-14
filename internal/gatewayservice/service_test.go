package gatewayservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

type recordingRunner struct {
	mu                       sync.Mutex
	calls                    [][]string
	unit                     string
	uid                      int
	loaded, active, holdStop bool
	failed, holdReload       bool
	guiName                  string
	dropins                  string
	faultOperation           string
	faultResult              Result
	blockOperation           string
	afterReload              func() error
	afterStop                func()
	immediateExit            bool
}

func (r *recordingRunner) Run(ctx context.Context, _ string, args []string) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	r.calls = append(r.calls, slices.Clone(args))
	operation := args[0]
	if operation == "--user" {
		operation = args[1]
	}
	if operation == "--no-pager" || operation == "--no-block" {
		operation = args[2]
	}
	if operation == r.faultOperation {
		return r.faultResult, nil
	}
	if operation == r.blockOperation {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	if operation == "managername" || operation == "manageruid" || operation == "list" || operation == "show" {
		return r.observe(operation)
	}
	return r.mutate(operation)
}

func (r *recordingRunner) observe(operation string) (Result, error) {
	switch operation {
	case "managername":
		return Result{Stdout: []byte(r.guiName + "\n")}, nil
	case "manageruid":
		return Result{Stdout: []byte(strconv.Itoa(r.uid) + "\n")}, nil
	case "list":
		if r.loaded {
			return Result{Stdout: []byte("opaque manager diagnostic must not be parsed")}, nil
		}
		return Result{ExitCode: 113}, nil
	case "show":
		return r.linuxObservation()
	default:
		return Result{}, fmt.Errorf("unexpected observation %q", operation)
	}
}

func (r *recordingRunner) mutate(operation string) (Result, error) {
	switch operation {
	case "daemon-reload":
		if !r.holdReload {
			_, err := os.Stat(r.unit)
			r.loaded = err == nil
		}
		if r.afterReload != nil {
			return Result{}, r.afterReload()
		}
	case "start", "bootstrap", "kickstart":
		r.loaded, r.active, r.failed = true, true, false
		if r.immediateExit {
			r.active = false
			if operation != "start" {
				r.loaded = false
			}
		}
	case "stop":
		if !r.holdStop {
			r.active = false
		}
		if r.afterStop != nil {
			r.afterStop()
		}
	case "bootout":
		if !r.holdStop {
			r.loaded, r.active = false, false
		}
	case "enable", "disable":
	default:
		return Result{}, fmt.Errorf("unexpected manager operation %q", operation)
	}
	return Result{}, nil
}

func (r *recordingRunner) linuxObservation() (Result, error) {
	active := "inactive"
	if r.failed {
		active = "failed"
	}
	if r.active {
		active = "active"
	}
	if !r.loaded {
		// systemd 255.4 retains failed history after reload, but clears the
		// vanished definition and returns default safety settings (real probe).
		return Result{Stdout: fmt.Appendf(nil, "LoadState=not-found\nActiveState=%s\nFragmentPath=\nDropInPaths=\nMainPID=0\nControlPID=0\nNeedDaemonReload=no\nType=\nExitType=main\nDelegate=no\nKillMode=control-group\nSendSIGKILL=yes\nTimeoutStopUSec=1min 30s\nRestart=no\n", active)}, nil
	}
	needReload := "no"
	if _, err := os.Stat(r.unit); errors.Is(err, os.ErrNotExist) {
		needReload = "yes"
	} else if err != nil {
		return Result{}, err
	}
	return Result{Stdout: fmt.Appendf(nil, "LoadState=loaded\nActiveState=%s\nFragmentPath=%s\nDropInPaths=%s\nMainPID=0\nControlPID=0\nNeedDaemonReload=%s\nType=exec\nExitType=main\nDelegate=yes\nKillMode=process\nSendSIGKILL=no\nTimeoutStopUSec=infinity\nRestart=no\n", active, r.unit, r.dropins, needReload)}, nil
}

func TestServiceStartAcknowledgementDoesNotProveActiveRegistration(t *testing.T) {
	for _, platform := range []Platform{Linux, MacOS} {
		t.Run(string(platform), func(t *testing.T) {
			f := newServiceFixture(t, platform)
			if _, err := f.manager.Install(f.ctx); err != nil {
				t.Fatal(err)
			}
			f.runner.immediateExit = true
			status, err := f.manager.Start(f.ctx)
			if !errors.Is(err, ErrManager) || (status.State != StateInactive && status.State != StateAbsent) {
				t.Fatalf("inactive start reported success: %+v %v", status, err)
			}
			if _, err = os.Stat(f.manager.unitPath()); err != nil {
				t.Fatalf("failed start removed recoverable service: %v", err)
			}
		})
	}
}

type serviceFixture struct {
	manager  *Manager
	runner   *recordingRunner
	inactive atomic.Bool
	ctx      context.Context
}

func TestServiceCleanupSurvivesRemovedOrRedirectedSavedExecutable(t *testing.T) {
	for _, platform := range []Platform{Linux, MacOS} {
		for _, redirect := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/redirect=%t", platform, redirect), func(t *testing.T) {
				testServiceCleanupSavedExecutable(t, platform, redirect)
			})
		}
	}
}

func serviceWithReplaceableExecutable(t *testing.T, platform Platform) *serviceFixture {
	t.Helper()
	f := newServiceFixture(t, platform)
	c := f.manager.config
	c.Executable = filepath.Join(filepath.Dir(c.ProfileDirectory), "delegate-old")
	if err := os.WriteFile(c.Executable, []byte("fixture source; never executed\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var err error
	f.manager, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

func testServiceCleanupSavedExecutable(t *testing.T, platform Platform, redirect bool) {
	t.Helper()
	f := serviceWithReplaceableExecutable(t, platform)
	c := f.manager.config
	retained := filepath.Join(c.ProfileDirectory, "retained-context")
	if err := os.WriteFile(retained, []byte("preserved identity and pin data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	removeOrRedirectServiceExecutable(t, c.Executable, c.ManagerExecutable, redirect)
	manager, err := New(c)
	if err != nil {
		t.Fatalf("saved source prevented cleanup construction: %v", err)
	}
	if _, err = manager.Status(f.ctx); err != nil {
		t.Fatalf("saved source prevented status: %v", err)
	}
	before := len(f.runner.calls)
	for _, action := range []func(context.Context) (Status, error){manager.Install, manager.Start} {
		if _, err = action(f.ctx); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("new launch accepted unavailable/redirected source: %v", err)
		}
	}
	if len(f.runner.calls) != before {
		t.Fatal("source refusal invoked the service manager")
	}
	if _, err = manager.Stop(f.ctx); err != nil {
		t.Fatalf("saved source prevented stop: %v", err)
	}
	if _, err = manager.Uninstall(f.ctx); err != nil {
		t.Fatalf("saved source prevented uninstall: %v", err)
	}
	data, err := os.ReadFile(retained)
	if err != nil || string(data) != "preserved identity and pin data\n" {
		t.Fatalf("cleanup altered retained profile bytes: %q %v", data, err)
	}
}

func removeOrRedirectServiceExecutable(t *testing.T, source, target string, redirect bool) {
	t.Helper()
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if redirect {
		if err := os.Symlink(target, source); err != nil {
			t.Fatal(err)
		}
	}
}

func newServiceFixture(t *testing.T, platform Platform) *serviceFixture {
	t.Helper()
	root, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "private-profile")
	if err = os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := verifiedexec.LocatePath("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	f := &serviceFixture{runner: &recordingRunner{uid: os.Getuid(), guiName: "Aqua"}}
	f.inactive.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	f.ctx = ctx
	t.Cleanup(cancel)
	f.manager, err = New(Config{Platform: platform, ProfileDirectory: profile, Executable: executable, Directory: filepath.Join(root, "units"), ManagerExecutable: executable, UID: os.Getuid(), Path: "/usr/local/bin:/usr/bin:/bin", Runner: f.runner, RuntimeInactive: func(context.Context) (bool, error) { return f.inactive.Load(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	f.runner.unit = f.manager.unitPath()
	return f
}

func TestServiceInstallStartStopUninstallRetainsProfile(t *testing.T) {
	for _, platform := range []Platform{Linux, MacOS} {
		t.Run(string(platform), func(t *testing.T) { testServiceLifecycle(t, platform) })
	}
}

func testServiceLifecycle(t *testing.T, platform Platform) {
	t.Helper()
	f := newServiceFixture(t, platform)
	assertServiceInstallIdempotent(t, f)
	status, err := f.manager.Start(f.ctx)
	if err != nil || (status.State != StateRunning && status.State != StateRegistered) {
		t.Fatalf("start: %+v %v", status, err)
	}
	status, err = f.manager.Stop(f.ctx)
	if err != nil || !status.StopRequested || (status.State != StateInactive && status.State != StateAbsent) {
		t.Fatalf("stop: %+v %v", status, err)
	}
	assertServiceUninstall(t, f)
	assertServiceCommands(t, f)
}

func assertServiceInstallIdempotent(t *testing.T, f *serviceFixture) {
	t.Helper()
	status, err := f.manager.Install(f.ctx)
	if err != nil || !status.Installed {
		t.Fatalf("install: %+v %v", status, err)
	}
	before, err := os.ReadFile(f.manager.unitPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(f.manager.unitPath())
	if err != nil || string(before) != string(after) {
		t.Fatalf("idempotent install changed definition: %v", err)
	}
}

func assertServiceUninstall(t *testing.T, f *serviceFixture) {
	t.Helper()
	status, err := f.manager.Uninstall(f.ctx)
	if err != nil || status.Installed {
		t.Fatalf("uninstall: %+v %v", status, err)
	}
	if _, err = os.Stat(f.manager.config.ProfileDirectory); err != nil {
		t.Fatalf("uninstall removed profile: %v", err)
	}
	if _, err = os.Lstat(f.manager.unitPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uninstall retained unit: %v", err)
	}
}

func assertServiceCommands(t *testing.T, f *serviceFixture) {
	t.Helper()
	f.runner.mu.Lock()
	defer f.runner.mu.Unlock()
	want := []string{"--user", "--no-block", "stop", f.manager.filename}
	if f.manager.config.Platform == MacOS {
		want = []string{"bootout", f.manager.target()}
	}
	if !slices.ContainsFunc(f.runner.calls, func(args []string) bool { return slices.Equal(args, want) }) {
		t.Fatalf("missing exact manager stop: %v", f.runner.calls)
	}
	for _, args := range f.runner.calls {
		if slices.Contains(args, "-k") || slices.Contains(args, "kill") || slices.Contains(args, "--force") {
			t.Fatalf("unsafe stop argv: %v", args)
		}
	}
}

func TestServiceUninstallRequiresManagerAndRuntimeInactivity(t *testing.T) {
	for _, hold := range []string{"manager", "runtime"} {
		t.Run(hold, func(t *testing.T) { testPendingServiceUninstall(t, hold) })
	}
}

func testPendingServiceUninstall(t *testing.T, hold string) {
	t.Helper()
	f := newServiceFixture(t, Linux)
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	manager := f.manager
	if hold == "manager" {
		f.runner.holdStop = true
		f.runner.afterStop = cancel
	} else {
		c := f.manager.config
		c.RuntimeInactive = func(context.Context) (bool, error) { cancel(); return false, nil }
		var err error
		manager, err = New(c)
		if err != nil {
			t.Fatal(err)
		}
	}
	status, err := manager.Uninstall(ctx)
	if !errors.Is(err, ErrPending) || status.State != StatePending {
		t.Fatalf("unproven stop: %+v %v", status, err)
	}
	if _, err = os.Stat(f.manager.unitPath()); err != nil {
		t.Fatalf("pending stop removed unit: %v", err)
	}
}

func TestServiceRejectsForeignFilesAndOverrides(t *testing.T) {
	for _, fault := range []string{"bytes", "symlink", "mode", "dropin"} {
		t.Run(fault, func(t *testing.T) {
			f := newServiceFixture(t, Linux)
			if _, err := f.manager.Install(f.ctx); err != nil {
				t.Fatal(err)
			}
			mutateServiceFixture(t, f, fault)
			if _, err := f.manager.Uninstall(f.ctx); !errors.Is(err, ErrForeignService) {
				t.Fatalf("foreign %s accepted: %v", fault, err)
			}
			if _, err := os.Lstat(f.manager.unitPath()); err != nil {
				t.Fatalf("foreign unit removed: %v", err)
			}
		})
	}
}

func mutateServiceFixture(t *testing.T, f *serviceFixture, fault string) {
	t.Helper()
	var err error
	switch fault {
	case "bytes":
		err = os.WriteFile(f.manager.unitPath(), []byte("foreign unit"), 0o600)
	case "mode":
		err = os.Chmod(f.manager.unitPath(), 0o644)
	case "symlink":
		err = os.Remove(f.manager.unitPath())
		if err == nil {
			err = os.Symlink(filepath.Join(f.manager.config.ProfileDirectory, "foreign"), f.manager.unitPath())
		}
	case "dropin":
		f.runner.dropins = "/foreign/override.conf"
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestServiceMacPrerequisitesAndUnknownManagerErrors(t *testing.T) {
	f := newServiceFixture(t, MacOS)
	f.runner.guiName = "Background"
	if _, err := f.manager.Install(f.ctx); !errors.Is(err, ErrGUIRequired) {
		t.Fatalf("SSH context accepted: %v", err)
	}
	if _, err := os.Stat(f.manager.config.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GUI failure created directory: %v", err)
	}
	f.runner.guiName = "Aqua"
	for _, code := range []int{1, 3, 112} {
		f.runner.faultOperation = "list"
		f.runner.faultResult = Result{ExitCode: code}
		if status, err := f.manager.Status(f.ctx); err == nil || status.State != StateUnknown {
			t.Fatalf("code %d became absence: %+v %v", code, status, err)
		}
	}
}

func TestServiceInstallFailureRetainsExactRecoverableUnit(t *testing.T) {
	f := newServiceFixture(t, Linux)
	f.runner.faultOperation = "enable"
	f.runner.faultResult = Result{ExitCode: 1}
	if status, err := f.manager.Install(f.ctx); err == nil || !status.Installed {
		t.Fatalf("enable failure hidden: %+v %v", status, err)
	}
	if present, err := f.manager.readUnit(); err != nil || !present {
		t.Fatalf("retry unit lost: %v", err)
	}
	f.runner.faultOperation = ""
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatalf("explicit retry failed: %v", err)
	}
}

func TestServiceRuntimeObservationErrorRetainsUnit(t *testing.T) {
	f := newServiceFixture(t, Linux)
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	fault := errors.New("profile identity lease observation failed")
	c := f.manager.config
	c.RuntimeInactive = func(context.Context) (bool, error) { return false, fault }
	manager, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	status, err := manager.Uninstall(f.ctx)
	if !errors.Is(err, ErrPending) || !errors.Is(err, fault) || !status.Installed {
		t.Fatalf("runtime fault became inactivity: %+v %v", status, err)
	}
	if _, err = os.Stat(manager.unitPath()); err != nil {
		t.Fatalf("runtime fault removed unit: %v", err)
	}
}

func TestServiceRequiresFiniteContextAndPrivateProfile(t *testing.T) {
	f := newServiceFixture(t, Linux)
	if _, err := f.manager.Install(context.Background()); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("unbounded manager operation accepted: %v", err)
	}
	if _, err := os.Stat(f.manager.config.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unbounded operation created unit directory: %v", err)
	}
	if err := os.Chmod(f.manager.config.ProfileDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Status(f.ctx); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("unsafe existing profile accepted: %v", err)
	}
	if _, err := New(f.manager.config); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("unsafe profile accepted by new manager: %v", err)
	}
}

func TestServiceProfileLockRejectsConcurrentDecisionAndReplacement(t *testing.T) {
	f := newServiceFixture(t, Linux)
	entered, release, joined := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		if err := <-joined; err != nil {
			t.Error(err)
		}
	})
	go func() {
		joined <- f.manager.WithProfileLock(f.ctx, func() error { close(entered); <-release; return nil })
	}()
	select {
	case <-entered:
	case err := <-joined:
		joined <- err
		t.Fatalf("initial profile lock failed: %v", err)
	case <-f.ctx.Done():
		t.Fatal("initial profile lock did not enter")
	}
	ctx, cancel := context.WithTimeout(f.ctx, 40*time.Millisecond)
	defer cancel()
	if err := WithProfileLock(ctx, f.manager.config.ProfileDirectory, func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent decision bypassed lock: %v", err)
	}
	once.Do(func() { close(release) })
	// Join before changing the profile directory; cleanup owns the final result.
	err := <-joined
	joined <- err
	old := f.manager.config.ProfileDirectory + "-original"
	if err = os.Rename(f.manager.config.ProfileDirectory, old); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(f.manager.config.ProfileDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.Status(f.ctx); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("replacement profile accepted: %v", err)
	}
}

func TestServiceDefinitionEscapesSelectedArguments(t *testing.T) {
	c := Config{Executable: `/Applications/Delegate $%.app/native`, ProfileDirectory: `/Users/owner/profile & "quoted" $%`, Path: `/Users/owner/a%:/Users/$HOME/bin`}
	for _, platform := range []Platform{Linux, MacOS} {
		c.Platform = platform
		data, err := render(c, "safe-name")
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if platform == Linux {
			for _, expected := range []string{`Type=exec`, `ExitType=main`, `KillMode=process`, `SendSIGKILL=no`, `Delegate=yes`, `TimeoutStopSec=infinity`, `Restart=no`, `"/Applications/Delegate $$%%.app/native"`, `Environment="PATH=/Users/owner/a%%:/Users/$HOME/bin"`} {
				if !strings.Contains(text, expected) {
					t.Fatalf("missing %q: %s", expected, text)
				}
			}
		} else {
			for _, expected := range []string{`<key>AbandonProcessGroup</key><true/>`, `<key>ExitTimeOut</key><integer>0</integer>`, `<key>KeepAlive</key><false/>`, `profile &amp; &#34;quoted&#34; $%`} {
				if !strings.Contains(text, expected) {
					t.Fatalf("missing %q: %s", expected, text)
				}
			}
		}
	}
}

func TestServiceBoundedCommandOutput(t *testing.T) {
	output := &boundedOutput{}
	data := []byte(strings.Repeat("x", maxCommandOutput+1))
	count, err := output.Write(data)
	if err != nil || count != len(data) || len(output.bytes) != maxCommandOutput || !output.overflow {
		t.Fatalf("unbounded manager output: count%d length%d overflow%v err%v", count, len(output.bytes), output.overflow, err)
	}
}
