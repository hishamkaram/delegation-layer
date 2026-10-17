package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func canonicalWorkspaceTestDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	testBaseOID = "1111111111111111111111111111111111111111"
	testHeadOID = "2222222222222222222222222222222222222222"
	testTreeOID = "3333333333333333333333333333333333333333"
	testBlobOID = "4444444444444444444444444444444444444444"
)

type recordingGitRunner struct {
	calls   []*exec.Cmd
	handler func(*exec.Cmd) error
}

func (r *recordingGitRunner) Run(_ context.Context, command *exec.Cmd) error {
	r.calls = append(r.calls, command)
	if r.handler == nil {
		return nil
	}
	return r.handler(command)
}

type gitTestExitError int

func (e gitTestExitError) Error() string { return "git test command failed" }

func (e gitTestExitError) ExitCode() int { return int(e) }

func TestResolveSourceUsesCommittedBaseAndRecordsDirtyObservation(t *testing.T) {
	root := canonicalWorkspaceTestDir(t)
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "nested")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &recordingGitRunner{handler: func(command *exec.Cmd) error {
		args := command.Args[3:]
		switch {
		case slices.Equal(args, []string{"rev-parse", "--show-toplevel"}):
			return writeCommandOutput(command, root+"\n")
		case slices.Equal(args, []string{"rev-parse", "--git-dir"}), slices.Equal(args, []string{"rev-parse", "--git-common-dir"}):
			return writeCommandOutput(command, "../.git\n")
		case slices.Equal(args, []string{"rev-parse", "--show-object-format=storage"}):
			return writeCommandOutput(command, "sha1\n")
		case slices.Equal(args, []string{"rev-parse", "--verify", "--end-of-options", "main^{commit}"}):
			return writeCommandOutput(command, testBaseOID+"\n")
		case slices.Equal(args, []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}):
			return writeCommandOutput(command, " M nested/file\x00")
		default:
			return errors.New("unexpected Git command: " + strings.Join(args, " "))
		}
	}}
	git := Git{Runner: runner}
	source, err := git.ResolveSource(context.Background(), cwd, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := Source{
		Root:         root,
		CommonDir:    filepath.Join(root, ".git"),
		GitDir:       filepath.Join(root, ".git"),
		RelativeCwd:  "nested",
		BaseOID:      testBaseOID,
		ObjectFormat: "sha1",
		Dirty:        true,
	}
	if source != want {
		t.Fatalf("source = %+v, want %+v", source, want)
	}
	if !containsArgs(runner.calls, []string{"rev-parse", "--verify", "--end-of-options", "main^{commit}"}) {
		t.Fatal("base revision was not resolved after --end-of-options")
	}
}

func TestGitRejectsInheritedRepositoryRedirectionBeforeRunner(t *testing.T) {
	t.Setenv("GIT_DIR", filepath.Join(canonicalWorkspaceTestDir(t), "wrong.git"))
	runner := &recordingGitRunner{}
	git := Git{Runner: runner}
	_, err := git.ResolveSource(context.Background(), canonicalWorkspaceTestDir(t), "HEAD")
	if !errors.Is(err, ErrInvalidGitContext) {
		t.Fatalf("error = %v, want ErrInvalidGitContext", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner received %d commands after inherited redirection", len(runner.calls))
	}
}

func TestValidateGitEnvironmentRejectsInheritedRedirections(t *testing.T) {
	variables := []string{
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_COMMON_DIR",
		"GIT_DIR",
		"GIT_INDEX_FILE",
		"GIT_NAMESPACE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_OBJECT_DIRECTORY_RELATIVE",
		"GIT_WORK_TREE",
	}
	for _, variable := range variables {
		t.Run(variable, func(t *testing.T) {
			t.Setenv(variable, filepath.Join(canonicalWorkspaceTestDir(t), "redirect"))
			if err := ValidateGitEnvironment(); !errors.Is(err, ErrInvalidGitContext) {
				t.Fatalf("error = %v, want ErrInvalidGitContext", err)
			}
		})
	}
}

func TestValidateGitEnvironmentAllowsUnsetOrEmptyRedirections(t *testing.T) {
	t.Setenv("GIT_DIR", "")
	if err := ValidateGitEnvironment(); err != nil {
		t.Fatal(err)
	}
}

func TestParseWorktreeRegistrationsRejectsUnterminatedNULOutput(t *testing.T) {
	if _, err := parseWorktreeRegistrations([]byte("worktree /missing")); err == nil {
		t.Fatal("unterminated worktree output was accepted")
	}
}

func TestProvisionVerifiesLinkedCheckoutRegistrationAndBacklink(t *testing.T) {
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	checkoutPath := filepath.Join(canonicalWorkspaceTestDir(t), "checkout")
	adminDir := filepath.Join(gitDir, "worktrees", "managed")
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	runner := newRepositoryFixtureRunner(root, checkoutPath, adminDir, testBaseOID)
	git := Git{Runner: runner}
	checkout, err := git.Provision(context.Background(), source, checkoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if checkout.CheckoutPath != checkoutPath || checkout.Workdir != checkoutPath || checkout.AdminDir != adminDir || checkout.AdminBacklink != adminDir {
		t.Fatalf("checkout identity = %+v", checkout)
	}
	if !containsArgs(runner.calls, []string{"worktree", "add", "--detach", "--lock", checkoutPath, testBaseOID}) {
		t.Fatal("provision did not use detached locked worktree creation")
	}
}

func TestInspectProvisionedReconcilesExistingDetachedLockedCheckout(t *testing.T) {
	fixture := newCheckoutFixture(t)
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	git := Git{Runner: runner}
	got, err := git.InspectProvisioned(context.Background(), fixture.checkout.Source, fixture.checkout.CheckoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != fixture.checkout {
		t.Fatalf("checkout = %+v, want %+v", got, fixture.checkout)
	}
	if containsArgs(runner.calls, []string{"worktree", "add", "--detach", "--lock", fixture.checkout.CheckoutPath, testBaseOID}) {
		t.Fatal("inspection created a worktree")
	}
}

func TestInspectProvisionedRejectsMismatchedBaseAndRegistration(t *testing.T) {
	fixture := newCheckoutFixture(t)
	source := fixture.checkout.Source
	source.BaseOID = testHeadOID
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	_, err := (Git{Runner: runner}).InspectProvisioned(context.Background(), source, fixture.checkout.CheckoutPath)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v, want ErrIdentityMismatch", err)
	}
}

func TestInspectCleanupAndCaptureAllowMissingMappedWorkdir(t *testing.T) {
	fixture := newCheckoutFixture(t)
	nested := filepath.Join(fixture.checkout.CheckoutPath, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.checkout.Source.RelativeCwd = "nested"
	fixture.checkout.Workdir = nested
	if err := os.Remove(nested); err != nil {
		t.Fatal(err)
	}
	runner := captureRunner(fixture, "")
	git := Git{Runner: runner}
	got, err := git.InspectCleanup(context.Background(), fixture.checkout.Source, fixture.checkout.CheckoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workdir != nested {
		t.Fatalf("cleanup workdir = %q, want declared missing path %q", got.Workdir, nested)
	}
	if _, err := git.BuildCapture(context.Background(), fixture.checkout, fixture.scratch); err != nil {
		t.Fatalf("capture with missing mapped workdir: %v", err)
	}
	if _, err := git.InspectProvisioned(context.Background(), fixture.checkout.Source, fixture.checkout.CheckoutPath); err == nil {
		t.Fatal("provision inspection accepted a missing mapped workdir")
	}
}

func TestInspectCleanupRejectsEitherAdminBacklinkDirection(t *testing.T) {
	tests := []struct {
		name string
		edit func(t *testing.T, fixture checkoutFixture)
	}{
		{
			name: "checkout gitdir backlink",
			edit: func(t *testing.T, fixture checkoutFixture) {
				otherAdmin := filepath.Join(fixture.root, ".git", "worktrees", "other")
				if err := os.MkdirAll(otherAdmin, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fixture.checkout.CheckoutPath, ".git"), []byte("gitdir: "+otherAdmin+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "admin gitdir backlink",
			edit: func(t *testing.T, fixture checkoutFixture) {
				otherCheckout := filepath.Join(canonicalWorkspaceTestDir(t), "other")
				if err := os.Mkdir(otherCheckout, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(otherCheckout, ".git"), []byte("foreign\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fixture.checkout.AdminDir, "gitdir"), []byte(filepath.Join(otherCheckout, ".git")+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCheckoutFixture(t)
			test.edit(t, fixture)
			runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
			_, err := (Git{Runner: runner}).InspectCleanup(context.Background(), fixture.checkout.Source, fixture.checkout.CheckoutPath)
			if !errors.Is(err, ErrIdentityMismatch) {
				t.Fatalf("error = %v, want ErrIdentityMismatch", err)
			}
			if containsArgs(runner.calls, []string{"worktree", "unlock", fixture.checkout.CheckoutPath}) || containsArgs(runner.calls, []string{"worktree", "remove", "--force", fixture.checkout.CheckoutPath}) {
				t.Fatal("read-only cleanup inspection attempted native cleanup")
			}
		})
	}
}

func TestInspectCleanupRejectsMalformedAdminBacklink(t *testing.T) {
	fixture := newCheckoutFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.checkout.AdminDir, "gitdir"), []byte("not-a-path\nextra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	_, err := (Git{Runner: runner}).InspectCleanup(context.Background(), fixture.checkout.Source, fixture.checkout.CheckoutPath)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v, want typed identity mismatch", err)
	}
}

func TestInspectCleanupRequiresExactNativeRegistration(t *testing.T) {
	fixture := newCheckoutFixture(t)
	unrelated := filepath.Join(canonicalWorkspaceTestDir(t), "unrelated")
	if err := os.Mkdir(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	baseHandler := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"worktree", "list", "--porcelain", "-z"}) {
			return writeCommandOutput(command, "worktree "+fixture.root+"\x00HEAD "+testBaseOID+"\x00worktree "+unrelated+"\x00HEAD "+testHeadOID+"\x00")
		}
		return baseHandler(command)
	}
	_, err := (Git{Runner: runner}).InspectCleanup(context.Background(), fixture.checkout.Source, fixture.checkout.CheckoutPath)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v, want ErrIdentityMismatch", err)
	}
	if containsArgs(runner.calls, []string{"worktree", "remove", "--force", fixture.checkout.CheckoutPath}) {
		t.Fatal("inspection attempted to remove an unrelated registration")
	}
}

func TestInspectCleanupClassifiesMalformedNativeRegistrationAsIdentity(t *testing.T) {
	fixture := newCheckoutFixture(t)
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	baseHandler := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, []string{"worktree", "list", "--porcelain", "-z"}) {
			return writeCommandOutput(command, "worktree "+fixture.checkout.CheckoutPath)
		}
		return baseHandler(command)
	}
	_, err := (Git{Runner: runner}).InspectCleanup(context.Background(), fixture.checkout.Source, fixture.checkout.CheckoutPath)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v, want typed ErrIdentityMismatch", err)
	}
}

func TestRemoveRetriesAfterUnlockWithoutUnlockingAgain(t *testing.T) {
	fixture := newCheckoutFixture(t)
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	state := &removeRecoveryState{fixture: fixture, baseHandler: runner.handler, locked: true, failRemove: true}
	runner.handler = state.handle
	git := Git{Runner: runner}
	if err := git.Remove(context.Background(), fixture.checkout); err == nil {
		t.Fatal("first removal unexpectedly succeeded")
	}
	if err := git.Remove(context.Background(), fixture.checkout); err != nil {
		t.Fatalf("retry removal: %v", err)
	}
	if state.unlockCalls != 1 || state.removeCalls != 2 {
		t.Fatalf("unlock calls = %d, remove calls = %d, want 1 and 2", state.unlockCalls, state.removeCalls)
	}
}

type removeRecoveryState struct {
	fixture     checkoutFixture
	baseHandler func(*exec.Cmd) error
	locked      bool
	removed     bool
	failRemove  bool
	unlockCalls int
	removeCalls int
}

func (s *removeRecoveryState) handle(command *exec.Cmd) error {
	args := command.Args[3:]
	switch {
	case slices.Equal(args, []string{"worktree", "list", "--porcelain", "-z"}):
		output := "worktree " + s.fixture.root + "\x00HEAD " + testBaseOID + "\x00"
		if !s.removed {
			output += "worktree " + s.fixture.checkout.CheckoutPath + "\x00HEAD " + testBaseOID + "\x00"
			if s.locked {
				output += "locked\x00"
			}
		}
		return writeCommandOutput(command, output)
	case slices.Equal(args, []string{"worktree", "unlock", s.fixture.checkout.CheckoutPath}):
		s.unlockCalls++
		if !s.locked {
			return gitTestExitError(1)
		}
		s.locked = false
		return nil
	case slices.Equal(args, []string{"worktree", "remove", "--force", s.fixture.checkout.CheckoutPath}):
		s.removeCalls++
		if s.failRemove {
			s.failRemove = false
			return errors.New("simulated crash after unlock")
		}
		s.removed = true
		if err := os.RemoveAll(s.fixture.checkout.CheckoutPath); err != nil {
			return err
		}
		return nil
	default:
		return s.baseHandler(command)
	}
}

func TestProvisionRejectsCheckoutNestedInSourceRepository(t *testing.T) {
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "managed")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	checkoutPath := filepath.Join(parent, "checkout")
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	runner := newRepositoryFixtureRunner(root, checkoutPath, filepath.Join(gitDir, "worktrees", "managed"), testBaseOID)
	_, err := (Git{Runner: runner}).Provision(context.Background(), source, checkoutPath)
	if !errors.Is(err, ErrInvalidGitContext) {
		t.Fatalf("error = %v, want ErrInvalidGitContext", err)
	}
	if containsArgs(runner.calls, []string{"worktree", "add", "--detach", "--lock", checkoutPath, testBaseOID}) {
		t.Fatal("nested source checkout reached Git worktree creation")
	}
}
