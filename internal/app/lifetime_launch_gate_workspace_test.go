package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This fixture proves native descriptor inheritance through the production
// launch wrapper and command runner. It does not claim guardian scope closure.
type worktreeGateTestScope struct {
	ctx    context.Context
	runner string
	abort  bool
	facts  *worktreeGateTestFacts
}

type worktreeGateTestFacts struct {
	budgetRequested atomic.Bool
	budgetCompleted atomic.Bool
}

func nativeWorktreeGateScope(t *testing.T) worktreeGateTestScope {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	runner, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner, err = filepath.EvalSymlinks(runner)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return worktreeGateTestScope{ctx: ctx, runner: runner}
}

func nativeWorktreeGit(t *testing.T, scope worktreeGateTestScope, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	if err := scope.Run(command, execution.CommandHooks{}); err != nil {
		t.Fatalf("native Git setup: %v: %s", err, diagnostic.String())
	}
}

func nativeWorktreeGateRepository(t *testing.T, scope worktreeGateTestScope) (string, string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "source")
	if err = os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeWorktreeGit(t, scope, repository, "init", "--quiet")
	nativeWorktreeGit(t, scope, repository, "-c", "user.name=Delegate fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "base")
	common := filepath.Join(repository, ".git")
	hook := "#!/bin/sh\nprintf 'ready' >&4\nIFS= read -r token <&5 || exit 0\n"
	if err = os.WriteFile(filepath.Join(common, "hooks", "post-checkout"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	return root, repository, common
}

func TestNativeGitRetainsTransferredLeaseThroughLaunchWrapper(t *testing.T) {
	scope := nativeWorktreeGateScope(t)
	root, repository, common := nativeWorktreeGateRepository(t, scope)
	lease := nativeWorktreeTransferredLease(t, scope.ctx, common)
	readyReader, readyWriter := nativeWorktreeTestPipe(t)
	releaseReader, releaseWriter := nativeWorktreeTestPipe(t)
	command := exec.Command("git", "-C", repository, "worktree", "add", "--detach", "--lock", filepath.Join(root, "checkout"), "HEAD")
	command.ExtraFiles = []*os.File{lease, readyWriter, releaseReader}
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	done := make(chan error, 1)
	parentClosed := make(chan error, 1)
	go func() {
		done <- scope.Run(command, execution.CommandHooks{Started: func() error {
			closeErr := lease.Close()
			parentClosed <- closeErr
			return closeErr
		}})
	}()
	observeNativeGitLease(t, scope.ctx, common, readyReader, releaseWriter, parentClosed, done, &diagnostic)
}

func nativeWorktreeTransferredLease(t *testing.T, ctx context.Context, common string) *os.File {
	t.Helper()
	lock, err := taskdir.OpenDirectoryLock(common, taskdir.LockLevelRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeNativeWorktreeTestFile(t, lock.Close()) })
	if err = lock.LockEX(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := lock.TakeExclusiveFile()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeNativeWorktreeTestFile(t, lease.Close()) })
	return lease
}

func observeNativeGitLease(t *testing.T, ctx context.Context, common string, readyReader, releaseWriter *os.File, parentClosed, done <-chan error, diagnostic *bytes.Buffer) {
	t.Helper()
	ready := make(chan error, 1)
	go func() {
		var token [5]byte
		_, readErr := io.ReadFull(readyReader, token[:])
		if readErr == nil && string(token[:]) != "ready" {
			readErr = errors.New("native Git hook sent an invalid readiness token")
		}
		ready <- readErr
	}()
	peer, err := taskdir.OpenDirectoryLock(common, taskdir.LockLevelRun)
	if err != nil {
		closeNativeWorktreeTestFile(t, releaseWriter.Close())
		closeNativeWorktreeTestFile(t, <-done)
		t.Fatal(err)
	}
	t.Cleanup(func() { closeNativeWorktreeTestFile(t, peer.Close()) })
	var readyErr, commandErr, exclusionErr error
	finished, observed := false, false
	select {
	case readyErr = <-ready:
		observed = true
		readyErr = errors.Join(readyErr, awaitNativeParentReferenceClosure(ctx, parentClosed))
		exclusionErr = peer.LockEXNonblocking()
	case commandErr = <-done:
		finished = true
		readyErr = errors.New("Git exited before its post-checkout hook was ready")
	case <-ctx.Done():
		readyErr = ctx.Err()
	}
	closeNativeWorktreeTestFile(t, releaseWriter.Close())
	if !finished {
		commandErr = <-done
	}
	closeNativeWorktreeTestFile(t, readyReader.Close())
	if !observed {
		closeNativeWorktreeTestFile(t, <-ready)
	}
	if readyErr != nil || commandErr != nil || !errors.Is(exclusionErr, task.ErrLockBusy) {
		t.Fatalf("native lease: ready=%v, Git=%v, exclusion=%v, diagnostic=%s", readyErr, commandErr, exclusionErr, diagnostic.String())
	}
	if err = peer.LockEXNonblocking(); err != nil {
		t.Fatalf("Git's final close did not release its inherited lease: %v", err)
	}
}

func awaitNativeParentReferenceClosure(ctx context.Context, parentClosed <-chan error) error {
	select {
	case err := <-parentClosed:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestNativeLaunchGateAbortReleasesTransferredLeaseWithoutGit(t *testing.T) {
	scope := nativeWorktreeGateScope(t)
	scope.abort = true
	scope.facts = &worktreeGateTestFacts{}
	common, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lease := nativeWorktreeTransferredLease(t, scope.ctx, common)
	command := exec.Command("git", "--version")
	command.ExtraFiles = []*os.File{lease}
	var output bytes.Buffer
	command.Stdout = &output
	if err = scope.Run(command, execution.CommandHooks{Started: lease.Close}); err == nil || output.Len() != 0 {
		t.Fatalf("aborted gate executed Git: error=%v, stdout=%q", err, output.String())
	}
	if nativeWorktreeDeadlineProven(command, scope.facts) {
		t.Fatal("gate abort was falsely counted as native deadline termination")
	}
	peer, err := taskdir.OpenDirectoryLock(common, taskdir.LockLevelRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeNativeWorktreeTestFile(t, peer.Close()) })
	if err = peer.LockEXNonblocking(); err != nil {
		t.Fatalf("aborted wrapper retained its inherited lease: %v", err)
	}
}

func TestNativeGitFixtureDeadlineStopsBlockedCommandAndReleasesLease(t *testing.T) {
	scope := nativeWorktreeGateScope(t)
	ctx, cancel := context.WithTimeout(scope.ctx, 5*time.Second)
	defer cancel()
	scope.ctx = ctx
	scope.facts = &worktreeGateTestFacts{}
	common, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lease := nativeWorktreeTransferredLease(t, ctx, common)
	input, _ := nativeWorktreeTestPipe(t)
	command := exec.Command("git", "hash-object", "--stdin")
	command.Stdin = input
	command.ExtraFiles = []*os.File{lease}
	parentClosed := false
	err = scope.Run(command, execution.CommandHooks{Started: func() error {
		closeErr := lease.Close()
		parentClosed = closeErr == nil
		return closeErr
	}})
	if err == nil || !parentClosed || !nativeWorktreeDeadlineProven(command, scope.facts) {
		t.Fatalf("fixture deadline did not join the blocked native command: error=%v, parentClosed=%t, state=%v", err, parentClosed, command.ProcessState)
	}
	peer, err := taskdir.OpenDirectoryLock(common, taskdir.LockLevelRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeNativeWorktreeTestFile(t, peer.Close()) })
	if err = peer.LockEXNonblocking(); err != nil {
		t.Fatalf("deadline-terminated Git retained its lease: %v", err)
	}
}

func nativeWorktreeDeadlineProven(command *exec.Cmd, facts *worktreeGateTestFacts) bool {
	if command.ProcessState == nil || facts == nil || !facts.budgetRequested.Load() || !facts.budgetCompleted.Load() {
		return false
	}
	status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
	return ok && status.Signaled()
}

func TestNativeWorktreeFixtureRejectsStartAfterStopping(t *testing.T) {
	fixture := &worktreeGateTestLifetime{stopped: true}
	started := false
	err := fixture.StartCommand(exec.Command("unused"), func() error {
		started = true
		return nil
	})
	if err == nil || started {
		t.Fatal("stopped native fixture accepted another launch")
	}
}

func nativeWorktreeTestPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeNativeWorktreeTestFile(t, reader.Close())
		closeNativeWorktreeTestFile(t, writer.Close())
	})
	return reader, writer
}

func closeNativeWorktreeTestFile(t *testing.T, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, os.ErrClosed) {
		t.Error(err)
	}
}
