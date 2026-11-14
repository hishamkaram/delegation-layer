package gatewayservice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestServiceLinuxReloadRecoveryAfterFailedOwnedUnlink(t *testing.T) {
	f := newServiceFixture(t, Linux)
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.runner.failed = true
	f.runner.faultOperation = "daemon-reload"
	f.runner.faultResult = Result{ExitCode: 1}
	if _, err := f.manager.Uninstall(f.ctx); !errors.Is(err, ErrManager) {
		t.Fatalf("original reload failure was lost: %v", err)
	}
	if _, err := os.Lstat(f.manager.unitPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failure did not follow owned unlink: %v", err)
	}
	f.runner.faultOperation = ""
	reopened, err := New(f.manager.config)
	if err != nil {
		t.Fatal(err)
	}
	if status, retryErr := reopened.Uninstall(f.ctx); retryErr != nil || status.Installed {
		t.Fatalf("reopened uninstall stranded by stale cache: %+v %v", status, retryErr)
	}
	if !f.runner.failed {
		t.Fatal("reconciliation erased historical failed state")
	}
	if _, err = reopened.Install(f.ctx); err != nil {
		t.Fatalf("subsequent explicit installation failed: %v", err)
	}
	assertNoResetFailed(t, f.runner)
}

func cachedMissingService(t *testing.T) *serviceFixture {
	t.Helper()
	f := newServiceFixture(t, Linux)
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.manager.unitPath()); err != nil {
		t.Fatal(err)
	}
	f.runner.failed = true
	return f
}

func assertRecoveryCalls(t *testing.T, runner *recordingRunner, after, reloads int) {
	t.Helper()
	runner.mu.Lock()
	defer runner.mu.Unlock()
	var observed int
	for _, args := range runner.calls[after:] {
		if slices.Contains(args, "daemon-reload") {
			observed++
			continue
		}
		if !slices.Contains(args, "show") {
			t.Fatalf("absent cache caused a targeted control: %v", args)
		}
	}
	if observed != reloads {
		t.Fatalf("recovery reloads=%d, want %d", observed, reloads)
	}
}

func assertNoResetFailed(t *testing.T, runner *recordingRunner) {
	t.Helper()
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, args := range runner.calls {
		if slices.Contains(args, "reset-failed") {
			t.Fatal("service recovery erased failure history")
		}
	}
}

func TestServiceLinuxCrashCacheReconcilesOnlyForInitialMutation(t *testing.T) {
	for _, action := range []string{"stop", "uninstall", "install"} {
		t.Run(action, func(t *testing.T) { testCrashCacheMutation(t, action) })
	}
}

func testCrashCacheMutation(t *testing.T, action string) {
	t.Helper()
	f := cachedMissingService(t)
	before := len(f.runner.calls)
	var reloads int
	f.runner.afterReload = func() error {
		reloads++
		_, readErr := os.Lstat(f.manager.unitPath())
		if reloads == 1 && !errors.Is(readErr, os.ErrNotExist) {
			return errors.New("recovery acted after a definition reappeared")
		}
		if reloads > 1 && readErr != nil {
			return errors.New("publication reload lacked its owned definition")
		}
		return nil
	}
	var status Status
	var err error
	switch action {
	case "stop":
		status, err = f.manager.Stop(f.ctx)
	case "uninstall":
		status, err = f.manager.Uninstall(f.ctx)
	case "install":
		status, err = f.manager.Install(f.ctx)
	default:
		t.Fatal("unknown fixture action")
	}
	if err != nil {
		t.Fatal(err)
	}
	if action == "install" {
		if !status.Installed || reloads != 2 {
			t.Fatal("fresh exact definition was not installed")
		}
		assertPublishedSingleLink(t, f.manager)
	} else {
		if status.Installed || status.State != StateAbsent {
			t.Fatalf("failed history was not an absent definition: %+v", status)
		}
		assertRecoveryCalls(t, f.runner, before, 1)
	}
	assertNoResetFailed(t, f.runner)
}

func TestServiceLinuxStatusAndMissingStartNeverReconcileCache(t *testing.T) {
	f := cachedMissingService(t)
	before := len(f.runner.calls)
	for _, action := range []func(context.Context) (Status, error){f.manager.Status, f.manager.Start} {
		if _, err := action(f.ctx); !errors.Is(err, ErrManagerCacheStale) {
			t.Fatalf("unowned cached definition was accepted: %v", err)
		}
	}
	assertRecoveryCalls(t, f.runner, before, 0)
}

type editingPropertiesRunner struct {
	runner Runner
	old    string
	new    string
}

func (r editingPropertiesRunner) Run(ctx context.Context, executable string, args []string) (Result, error) {
	result, err := r.runner.Run(ctx, executable, args)
	if err == nil && slices.Contains(args, "show") {
		result.Stdout = []byte(strings.ReplaceAll(string(result.Stdout), r.old, r.new))
	}
	return result, err
}

func TestServiceLinuxCacheHintRefusesUnsafeOrAmbiguousProperties(t *testing.T) {
	cases := []struct{ name, old, replacement string }{
		{"active", "ActiveState=failed\n", "ActiveState=active\n"},
		{"dropin", "DropInPaths=\n", "DropInPaths=/foreign.conf\n"},
		{"fragment", "FragmentPath=", "FragmentPath=/foreign"},
		{"safety", "KillMode=process\n", "KillMode=control-group\n"},
		{"main-pid", "MainPID=0\n", "MainPID=9\n"},
		{"control-pid", "ControlPID=0\n", "ControlPID=9\n"},
		{"no-reload-hint", "NeedDaemonReload=yes\n", "NeedDaemonReload=no\n"},
		{"missing-hint", "NeedDaemonReload=yes\n", ""},
		{"missing-safety", "KillMode=process\n", ""},
		{"duplicate", "LoadState=loaded\n", "LoadState=loaded\nLoadState=loaded\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := cachedMissingService(t)
			c := f.manager.config
			c.Runner = editingPropertiesRunner{runner: f.runner, old: tc.old, new: tc.replacement}
			manager, err := New(c)
			if err != nil {
				t.Fatal(err)
			}
			before := len(f.runner.calls)
			if _, err = manager.Uninstall(f.ctx); err == nil {
				t.Fatal("unsafe or ambiguous cache hint was accepted")
			}
			assertRecoveryCalls(t, f.runner, before, 0)
		})
	}
}

func TestServiceLinuxCacheReloadFailureTimeoutAndStillLoadedFailClosed(t *testing.T) {
	for _, condition := range []string{"failure", "timeout", "still-loaded"} {
		t.Run(condition, func(t *testing.T) {
			f := cachedMissingService(t)
			ctx := f.ctx
			var expected error
			switch condition {
			case "failure":
				f.runner.faultOperation = "daemon-reload"
				f.runner.faultResult = Result{ExitCode: 1}
				expected = ErrManager
			case "timeout":
				f.runner.blockOperation = "daemon-reload"
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				expected = context.DeadlineExceeded
			case "still-loaded":
				f.runner.holdReload = true
				expected = ErrManagerCacheStale
			default:
				t.Fatal("unknown fixture condition")
			}
			before := len(f.runner.calls)
			if _, err := f.manager.Uninstall(ctx); !errors.Is(err, expected) {
				t.Fatalf("recovery failure was lost: %v", err)
			}
			assertRecoveryCalls(t, f.runner, before, 1)
		})
	}
}

func TestServiceLinuxCacheReloadReappearingDefinitionIsNeverAdopted(t *testing.T) {
	for _, valid := range []bool{false, true} {
		name := "foreign"
		if valid {
			name = "exact"
		}
		t.Run(name, func(t *testing.T) {
			f := cachedMissingService(t)
			contents := []byte("foreign concurrent definition\n")
			expected := ErrForeignService
			if valid {
				contents = slices.Clone(f.manager.rendered)
				expected = ErrServiceChanged
			}
			f.runner.afterReload = func() error { return os.WriteFile(f.manager.unitPath(), contents, 0o600) }
			before := len(f.runner.calls)
			if _, err := f.manager.Uninstall(f.ctx); !errors.Is(err, expected) {
				t.Fatalf("reappearing definition was adopted: %v", err)
			}
			assertRecoveryCalls(t, f.runner, before, 1)
			data, err := os.ReadFile(f.manager.unitPath())
			if err != nil || !slices.Equal(data, contents) {
				t.Fatalf("reappearing definition was modified: %v", err)
			}
		})
	}
}

type showSideEffectRunner struct {
	runner Runner
	after  func() error
}

func (r showSideEffectRunner) Run(ctx context.Context, executable string, args []string) (Result, error) {
	result, err := r.runner.Run(ctx, executable, args)
	if err == nil && slices.Contains(args, "show") {
		err = r.after()
	}
	return result, err
}

func TestServiceLinuxCacheReloadRechecksLateDefinitionReappearance(t *testing.T) {
	for _, phase := range []string{"refreshed-show", "stop-poll"} {
		for _, valid := range []bool{false, true} {
			name := phase + "/foreign"
			if valid {
				name = phase + "/exact"
			}
			t.Run(name, func(t *testing.T) { testLateCacheDefinition(t, phase, valid) })
		}
	}
}

func testLateCacheDefinition(t *testing.T, phase string, valid bool) {
	t.Helper()
	f := cachedMissingService(t)
	contents := []byte("foreign late replacement\n")
	expected := ErrForeignService
	if valid {
		contents, expected = slices.Clone(f.manager.rendered), ErrServiceChanged
	}
	show := 0
	trigger := 2
	if phase == "stop-poll" {
		trigger = 3
		f.inactive.Store(false)
	}
	c := f.manager.config
	c.Runner = showSideEffectRunner{runner: f.runner, after: func() error {
		show++
		if show == trigger {
			return os.WriteFile(f.manager.unitPath(), contents, 0o600)
		}
		return nil
	}}
	manager, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.runner.calls)
	if _, err = manager.Uninstall(f.ctx); !errors.Is(err, expected) {
		t.Fatalf("late definition was adopted: %v", err)
	}
	assertRecoveryCalls(t, f.runner, before, 1)
	data, err := os.ReadFile(f.manager.unitPath())
	if err != nil || !slices.Equal(data, contents) {
		t.Fatalf("late replacement was changed: %v", err)
	}
}

func TestServiceLinuxCacheRecoveryStillRequiresRuntimeAndDurableAbsence(t *testing.T) {
	for _, condition := range []string{"runtime-busy", "barrier-failure"} {
		t.Run(condition, func(t *testing.T) {
			f := cachedMissingService(t)
			ctx := f.ctx
			expected := errors.New("injected absence barrier failure")
			if condition == "runtime-busy" {
				f.inactive.Store(false)
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				expected = ErrPending
			} else {
				fault := &serviceBarrierFault{path: f.manager.config.Directory, err: expected}
				f.manager.directoryBarrier = fault.barrier
			}
			before := len(f.runner.calls)
			if _, err := f.manager.Uninstall(ctx); !errors.Is(err, expected) {
				t.Fatalf("reconciliation bypassed closure/durability prerequisite: %v", err)
			}
			assertRecoveryCalls(t, f.runner, before, 1)
		})
	}
}

func TestServiceLinuxCacheRecoveryRefusesLinkedExistingDefinitions(t *testing.T) {
	for _, link := range []string{"symlink", "hardlink"} {
		t.Run(link, func(t *testing.T) {
			f := cachedMissingService(t)
			other := filepath.Join(f.manager.config.ProfileDirectory, "foreign-linked-definition")
			if err := os.WriteFile(other, f.manager.rendered, 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			if link == "symlink" {
				err = os.Symlink(other, f.manager.unitPath())
			} else {
				err = os.Link(other, f.manager.unitPath())
			}
			if err != nil {
				t.Fatal(err)
			}
			before := len(f.runner.calls)
			if _, err = f.manager.Uninstall(f.ctx); !errors.Is(err, ErrForeignService) {
				t.Fatalf("linked existing definition admitted cache recovery: %v", err)
			}
			assertRecoveryCalls(t, f.runner, before, 0)
			if _, err = os.Lstat(f.manager.unitPath()); err != nil {
				t.Fatalf("foreign link was removed: %v", err)
			}
		})
	}
}
