package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func topologyTestSource(t *testing.T) Source {
	t.Helper()
	root := canonicalWorkspaceTestDir(t)
	common := filepath.Join(root, ".git")
	if err := os.Mkdir(common, 0o755); err != nil {
		t.Fatal(err)
	}
	return Source{Root: root, CommonDir: common, GitDir: common}
}

func topologyTestLock(t *testing.T, path string) *taskdir.LockFile {
	t.Helper()
	lock, err := taskdir.OpenDirectoryLock(path, taskdir.LockLevelRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	return lock
}

func requireTopologyPeerBlocked(t *testing.T, common string) {
	t.Helper()
	peer := topologyTestLock(t, common)
	if err := peer.LockEXNonblocking(); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("peer entered the same Git topology: %v", err)
	}
}

func requireTopologyLeaseReleased(t *testing.T, common string) {
	t.Helper()
	peer := topologyTestLock(t, common)
	if err := peer.LockEXNonblocking(); err != nil {
		t.Fatalf("completed command retained Git topology lease: %v", err)
	}
}

func TestWorktreeMetadataCommandsExcludeOtherOwners(t *testing.T) {
	for _, action := range []string{"add", "list", "unlock", "remove"} {
		t.Run(action, func(t *testing.T) {
			source := topologyTestSource(t)
			runner := topologyTestRunner(t, source, action)
			git := Git{Runner: runner}
			directory := source.CommonDir
			if action == "add" {
				directory = source.Root
			}
			var err error
			if action == "list" {
				_, err = git.worktreeRegistrations(t.Context(), source)
			} else {
				err = git.runWorktreeCommand(t.Context(), source, directory, []string{"worktree", action})
			}
			if err != nil || len(runner.calls) != 1 {
				t.Fatalf("metadata command = %v, calls = %d", err, len(runner.calls))
			}
			requireTopologyLeaseReleased(t, source.CommonDir)
		})
	}
}

func topologyTestRunner(t *testing.T, source Source, action string) *recordingGitRunner {
	t.Helper()
	return &recordingGitRunner{handler: func(command *exec.Cmd) error {
		directory := source.CommonDir
		if action == "add" {
			directory = source.Root
		}
		if command.Dir != directory || command.Args[2] != directory {
			t.Fatalf("worktree %s directory = %q/%q, want %q", action, command.Dir, command.Args[2], directory)
		}
		requireTopologyPeerBlocked(t, source.CommonDir)
		if len(command.ExtraFiles) != 1 {
			t.Fatal("Git did not inherit the topology lease")
		}
		info, err := command.ExtraFiles[0].Stat()
		if err != nil || !info.IsDir() {
			t.Fatalf("inherited lease is not the common directory: %v", err)
		}
		if action == "list" {
			return writeCommandOutput(command, string(worktreeInventoryBytes(source.Root)))
		}
		return nil
	}}
}

func TestWorktreeCommandsUseSourceContextOnlyForAdd(t *testing.T) {
	fixture := newCheckoutFixture(t)
	if err := os.RemoveAll(fixture.checkout.CheckoutPath); err != nil {
		t.Fatal(err)
	}
	source := fixture.checkout.Source
	runner := newRepositoryFixtureRunner(source.Root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	state := &removeRecoveryState{fixture: fixture, baseHandler: runner.handler, locked: true}
	seen := make(map[string]bool)
	runner.handler = func(command *exec.Cmd) error {
		if args := command.Args[3:]; len(args) >= 2 && args[0] == "worktree" {
			action := args[1]
			directory := source.CommonDir
			if action == "add" {
				directory = source.Root
			}
			if command.Dir != directory || command.Args[2] != directory {
				t.Fatalf("worktree %s directory = %q/%q, want %q", action, command.Dir, command.Args[2], directory)
			}
			requireTopologyPeerBlocked(t, source.CommonDir)
			seen[action] = true
		}
		return state.handle(command)
	}
	git := Git{Runner: runner}
	checkout, err := git.Provision(t.Context(), source, fixture.checkout.CheckoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = git.Remove(t.Context(), checkout); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"add", "list", "unlock", "remove"} {
		if !seen[action] {
			t.Fatalf("production provision/removal did not exercise worktree %s", action)
		}
	}
	requireTopologyLeaseReleased(t, source.CommonDir)
}

func TestWorktreeMetadataLeaseUsesDirectoryIdentityAcrossAliases(t *testing.T) {
	source := topologyTestSource(t)
	alias := worktreeParentAlias(t, source.CommonDir)
	runner := &recordingGitRunner{handler: func(*exec.Cmd) error {
		requireTopologyPeerBlocked(t, alias)
		other := topologyTestSource(t)
		requireTopologyLeaseReleased(t, other.CommonDir)
		return nil
	}}
	if err := (Git{Runner: runner}).runWorktreeCommand(t.Context(), source, source.CommonDir, []string{"worktree", "list"}); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeMetadataLeaseReleasesAfterRunnerFailure(t *testing.T) {
	source := topologyTestSource(t)
	want := errors.New("native Git launch failed")
	runner := &recordingGitRunner{handler: func(*exec.Cmd) error {
		requireTopologyPeerBlocked(t, source.CommonDir)
		return want
	}}
	err := (Git{Runner: runner}).runWorktreeCommand(t.Context(), source, source.Root, []string{"worktree", "add"})
	if !errors.Is(err, want) {
		t.Fatalf("native failure was lost: %v", err)
	}
	requireTopologyLeaseReleased(t, source.CommonDir)
}

func TestWorktreeMetadataLeaseRejectsExpiredDeadlineBeforeRunner(t *testing.T) {
	source := topologyTestSource(t)
	owner := topologyTestLock(t, source.CommonDir)
	if err := owner.LockEX(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	runner := &recordingGitRunner{}
	err := (Git{Runner: runner}).runWorktreeCommand(ctx, source, source.Root, []string{"worktree", "add"})
	if !errors.Is(err, context.DeadlineExceeded) || len(runner.calls) != 0 {
		t.Fatalf("expired command entered Git: %v, %d calls", err, len(runner.calls))
	}
}

type topologyWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *topologyWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestWorktreeMetadataContendedLeaseHonorsCancellationAndRelease(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "peer-released", true: "caller-canceled"}[cancelWait], func(t *testing.T) {
			source := topologyTestSource(t)
			owner := topologyTestLock(t, source.CommonDir)
			if err := owner.LockEX(t.Context()); err != nil {
				t.Fatal(err)
			}
			parent, end := context.WithTimeout(t.Context(), time.Minute)
			defer end()
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			observed := &topologyWaitContext{Context: ctx, waiting: make(chan struct{})}
			runner := &recordingGitRunner{}
			done := make(chan error, 1)
			go func() {
				done <- (Git{Runner: runner}).runWorktreeCommand(observed, source, source.Root, []string{"worktree", "add"})
			}()
			select {
			case <-observed.waiting:
			case err := <-done:
				t.Fatalf("Git command did not wait for its contended lease: %v", err)
			case <-parent.Done():
				t.Fatal(parent.Err())
			}
			if cancelWait {
				cancel()
			} else if err := owner.Unlock(); err != nil {
				t.Fatal(err)
			}
			assertTopologyWaitResult(t, <-done, runner, cancelWait)
		})
	}
}

func assertTopologyWaitResult(t *testing.T, err error, runner *recordingGitRunner, canceled bool) {
	t.Helper()
	if canceled {
		if !errors.Is(err, context.Canceled) || !errors.Is(err, task.ErrLockBusy) || len(runner.calls) != 0 {
			t.Fatalf("canceled contention entered Git or lost its cause: %v, calls=%d", err, len(runner.calls))
		}
		return
	}
	if err != nil || len(runner.calls) != 1 {
		t.Fatalf("released contention did not run exactly once: %v, calls=%d", err, len(runner.calls))
	}
}

func TestWorktreeMetadataRunnerPreservesExistingDescriptors(t *testing.T) {
	first, err := os.Open(canonicalWorkspaceTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := first.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	second, err := os.Open(canonicalWorkspaceTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := second.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	command := exec.Command("unused")
	command.ExtraFiles = []*os.File{first}
	runner := &recordingGitRunner{}
	wrapped := worktreeMetadataRunner{runner: runner, file: second}
	if err = wrapped.Run(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != command || !slices.Equal(command.ExtraFiles, []*os.File{first, second}) {
		t.Fatal("metadata runner replaced the command or its existing inherited descriptors")
	}
}
