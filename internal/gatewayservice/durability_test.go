package gatewayservice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type serviceBarrierFault struct {
	path     string
	err      error
	attempts []string
}

func (f *serviceBarrierFault) barrier(directory *os.File) error {
	path := filepath.Clean(directory.Name())
	f.attempts = append(f.attempts, path)
	if path == f.path {
		return f.err
	}
	return taskdir.BarrierFile(directory)
}

func nestedServiceFixture(t *testing.T, platform Platform) *serviceFixture {
	t.Helper()
	f := newServiceFixture(t, platform)
	c := f.manager.config
	c.Directory = filepath.Join(filepath.Dir(c.ProfileDirectory), "service-config", "intermediate", "units")
	var err error
	f.manager, err = New(c)
	if err != nil {
		t.Fatal(err)
	}
	f.runner.unit = f.manager.unitPath()
	return f
}

func TestServiceInstallRequiresAncestorBarriersOnCreationAndRetry(t *testing.T) {
	f := nestedServiceFixture(t, Linux)
	parent := filepath.Dir(f.manager.config.Directory)
	fault := &serviceBarrierFault{path: parent, err: errors.New("injected parent sync failure")}
	f.manager.directoryBarrier = fault.barrier
	if _, err := f.manager.Install(f.ctx); !errors.Is(err, fault.err) {
		t.Fatalf("install acknowledged without durable parent: %v", err)
	}
	assertPublishedSingleLink(t, f.manager)
	assertNoServiceEnable(t, f.runner)
	reopened, err := New(f.manager.config)
	if err != nil {
		t.Fatal(err)
	}
	reopened.directoryBarrier = fault.barrier
	if _, err = reopened.Install(f.ctx); !errors.Is(err, fault.err) {
		t.Fatalf("matching retry forgot failed parent barrier: %v", err)
	}
	assertNoServiceEnable(t, f.runner)
	fault.path = ""
	fault.attempts = nil
	if _, err = reopened.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	assertServiceAncestorBarriers(t, reopened, fault.attempts)
}

func assertNoServiceEnable(t *testing.T, runner *recordingRunner) {
	t.Helper()
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, args := range runner.calls {
		if slices.Contains(args, "enable") || slices.Contains(args, "daemon-reload") {
			t.Fatalf("manager mutated before installation durability: %v", args)
		}
	}
}

func assertServiceAncestorBarriers(t *testing.T, manager *Manager, attempts []string) {
	t.Helper()
	unitDirectory := manager.config.Directory
	for _, path := range []string{unitDirectory, filepath.Dir(unitDirectory), filepath.Dir(filepath.Dir(unitDirectory)), filepath.Dir(manager.config.ProfileDirectory), string(filepath.Separator)} {
		if !slices.Contains(attempts, path) {
			t.Fatalf("durable directory acknowledgment omitted %q: %v", path, attempts)
		}
	}
}

func TestServiceAbsentUninstallRetriesFailedRemovalBarrier(t *testing.T) {
	for _, missingDirectories := range []bool{false, true} {
		name := "unit-only"
		if missingDirectories {
			name = "missing-parents"
		}
		t.Run(name, func(t *testing.T) { testServiceAbsentUninstall(t, missingDirectories) })
	}
}

func testServiceAbsentUninstall(t *testing.T, missingDirectories bool) {
	t.Helper()
	f := nestedServiceFixture(t, MacOS)
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	profileData := filepath.Join(f.manager.config.ProfileDirectory, "retained-profile")
	if err := os.WriteFile(profileData, []byte("identity, pins and history stay owned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fault := &serviceBarrierFault{path: f.manager.config.Directory, err: errors.New("injected removal sync failure")}
	f.manager.directoryBarrier = fault.barrier
	if _, err := f.manager.Uninstall(f.ctx); !errors.Is(err, fault.err) {
		t.Fatalf("missing removal barrier error: %v", err)
	}
	if _, err := os.Lstat(f.manager.unitPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fault must follow actual unlink: %v", err)
	}
	if missingDirectories {
		removeServiceDirectories(t, f.manager.config.Directory)
		fault.path = filepath.Dir(f.manager.config.ProfileDirectory)
	}
	reopened, err := New(f.manager.config)
	if err != nil {
		t.Fatal(err)
	}
	reopened.directoryBarrier = fault.barrier
	if _, err = reopened.Uninstall(f.ctx); !errors.Is(err, fault.err) {
		t.Fatalf("already-absent retry acknowledged without barrier: %v", err)
	}
	fault.path = ""
	fault.attempts = nil
	status, err := reopened.Uninstall(f.ctx)
	if err != nil || status.Installed {
		t.Fatalf("durable absence retry: %+v %v", status, err)
	}
	if !slices.Contains(fault.attempts, filepath.Dir(reopened.config.ProfileDirectory)) {
		t.Fatal("absence retry omitted nearest existing parent")
	}
	data, err := os.ReadFile(profileData)
	if err != nil || string(data) != "identity, pins and history stay owned\n" {
		t.Fatalf("uninstall changed retained profile: %v", err)
	}
}

func removeServiceDirectories(t *testing.T, unitDirectory string) {
	t.Helper()
	for _, path := range []string{unitDirectory, filepath.Dir(unitDirectory), filepath.Dir(filepath.Dir(unitDirectory))} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServiceUninstallRechecksAbsenceAfterBarrier(t *testing.T) {
	f := newServiceFixture(t, MacOS)
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	var reappeared bool
	f.manager.directoryBarrier = func(directory *os.File) error {
		if err := taskdir.BarrierFile(directory); err != nil {
			return err
		}
		if directory.Name() == string(filepath.Separator) {
			reappeared = true
			return os.WriteFile(f.manager.unitPath(), f.manager.rendered, 0o600)
		}
		return nil
	}
	if _, err := f.manager.Uninstall(f.ctx); !errors.Is(err, ErrServiceChanged) {
		t.Fatalf("uninstall acknowledged a reappearing definition: %v", err)
	}
	if !reappeared {
		t.Fatal("fixture did not reach final ancestry barrier")
	}
	assertPublishedSingleLink(t, f.manager)
}

func TestServiceInstallRejectsReplacedAncestorWithUnchangedLeaf(t *testing.T) {
	f := nestedServiceFixture(t, Linux)
	var heldLeaf os.FileInfo
	f.manager.directoryBarrier = func(directory *os.File) error {
		if err := taskdir.BarrierFile(directory); err != nil {
			return err
		}
		if filepath.Clean(directory.Name()) != f.manager.config.Directory {
			return nil
		}
		var err error
		heldLeaf, err = directory.Stat()
		if err != nil {
			return err
		}
		ancestor := filepath.Dir(filepath.Dir(f.manager.config.Directory))
		parked := ancestor + "-old"
		if err = os.Rename(ancestor, parked); err != nil {
			return err
		}
		return os.Symlink(parked, ancestor)
	}
	if _, err := f.manager.Install(f.ctx); err == nil {
		t.Fatal("install acknowledged changed ancestor links")
	}
	after, err := os.Stat(f.manager.config.Directory)
	if err != nil || heldLeaf == nil || !os.SameFile(heldLeaf, after) {
		t.Fatalf("fixture must preserve the exact leaf through ancestor replacement: %v", err)
	}
	assertNoServiceEnable(t, f.runner)
}

func TestServiceLinuxUninstallReloadsAfterUnlinkBarrierFailure(t *testing.T) {
	for _, reloadFailure := range []bool{false, true} {
		name := "reload-succeeds"
		if reloadFailure {
			name = "reload-also-fails"
		}
		t.Run(name, func(t *testing.T) { testLinuxRemovalBarrier(t, reloadFailure) })
	}
}

func testLinuxRemovalBarrier(t *testing.T, reloadFailure bool) {
	t.Helper()
	f := nestedServiceFixture(t, Linux)
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	if reloadFailure {
		f.runner.faultOperation = "daemon-reload"
		f.runner.faultResult = Result{ExitCode: 1}
	}
	fault := &serviceBarrierFault{path: f.manager.config.Directory, err: errors.New("injected post-unlink barrier failure")}
	f.manager.directoryBarrier = fault.barrier
	if _, err := f.manager.Uninstall(f.ctx); !errors.Is(err, fault.err) || reloadFailure && !errors.Is(err, ErrManager) {
		t.Fatalf("unlink/reload failures were lost: %v", err)
	}
	if _, err := os.Lstat(f.manager.unitPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture did not reach actual unlink: %v", err)
	}
	f.runner.mu.Lock()
	loaded := f.runner.loaded
	var reloads int
	for _, args := range f.runner.calls {
		if slices.Contains(args, "daemon-reload") {
			reloads++
		}
	}
	f.runner.mu.Unlock()
	if reloads != 2 || loaded != reloadFailure {
		t.Fatalf("owned unlink did not reconcile manager cache: reloads=%d cached=%t", reloads, loaded)
	}
	f.runner.faultOperation = ""
	assertLinuxAbsentRemovalRetry(t, f, fault)
}

func assertLinuxAbsentRemovalRetry(t *testing.T, f *serviceFixture, fault *serviceBarrierFault) {
	t.Helper()
	reopened, err := New(f.manager.config)
	if err != nil {
		t.Fatal(err)
	}
	reopened.directoryBarrier = fault.barrier
	if _, err = reopened.Uninstall(f.ctx); !errors.Is(err, fault.err) || errors.Is(err, ErrForeignService) {
		t.Fatalf("reopened retry could not confirm absent cache and failed barrier: %v", err)
	}
	fault.path = ""
	status, err := reopened.Uninstall(f.ctx)
	if err != nil || status.Installed {
		t.Fatalf("reconciled retry did not finish durable absence: %+v %v", status, err)
	}
	if _, err = os.Stat(f.manager.config.ProfileDirectory); err != nil {
		t.Fatalf("uninstall removed profile: %v", err)
	}
}

type replacingDisableRunner struct {
	runner *recordingRunner
	path   string
}

func (r replacingDisableRunner) Run(ctx context.Context, executable string, args []string) (Result, error) {
	result, err := r.runner.Run(ctx, executable, args)
	if err == nil && slices.Contains(args, "disable") {
		err = os.WriteFile(r.path, []byte("foreign replacement after disable\n"), 0o600)
	}
	return result, err
}

func TestServiceLinuxUninstallDoesNotReloadOnPreUnlinkValidationFailure(t *testing.T) {
	f := newServiceFixture(t, Linux)
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	c := f.manager.config
	c.Runner = replacingDisableRunner{runner: f.runner, path: f.manager.unitPath()}
	manager, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Uninstall(f.ctx); !errors.Is(err, ErrForeignService) {
		t.Fatalf("pre-unlink validation did not fail: %v", err)
	}
	data, err := os.ReadFile(manager.unitPath())
	if err != nil || string(data) != "foreign replacement after disable\n" {
		t.Fatalf("foreign replacement was unlinked: %v", err)
	}
	f.runner.mu.Lock()
	defer f.runner.mu.Unlock()
	var reloads int
	for _, args := range f.runner.calls {
		if slices.Contains(args, "daemon-reload") {
			reloads++
		}
	}
	if reloads != 1 || !f.runner.loaded {
		t.Fatal("pre-unlink failure mutated the manager cache")
	}
}
