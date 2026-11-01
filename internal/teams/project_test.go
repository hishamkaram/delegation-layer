package teams

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

type nativeGitRunner struct{}

// These finite local Git fixture commands own no persistent children. The
// workspace facade remains the owner of command construction and environment.
func (nativeGitRunner) Run(ctx context.Context, command *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return command.Run()
}

func fixtureGit(t *testing.T) workspace.Git {
	t.Helper()
	executable, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("native Git required:", err)
	}
	return workspace.Git{
		Runner: nativeGitRunner{}, Executable: executable,
		Environment: []string{"PATH=" + filepath.Dir(executable), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"},
	}
}

func gitFixtureCommand(t *testing.T, git workspace.Git, cwd string, args ...string) {
	t.Helper()
	command := exec.Command(git.Executable, append([]string{"-C", cwd}, args...)...)
	command.Env = git.Environment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fixture Git failed: %v: %s", err, output)
	}
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func canonicalTempDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveProjectGitIdentityIncludesLinkedWorktrees(t *testing.T) {
	git := fixtureGit(t)
	root := canonicalTempDirectory(t)
	repository := mkdir(t, filepath.Join(root, "repo"))
	gitFixtureCommand(t, git, repository, "init", "-q")
	unborn, err := ResolveProject(t.Context(), repository, "", git)
	if err != nil || unborn.Kind != "git" || unborn.Path != filepath.Join(repository, ".git") {
		t.Fatalf("unborn repository: %+v %v", unborn, err)
	}
	gitFixtureCommand(t, git, repository, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "fixture")
	linked := filepath.Join(root, "linked")
	gitFixtureCommand(t, git, repository, "worktree", "add", "--detach", linked, "HEAD")
	nested := mkdir(t, filepath.Join(linked, "nested"))
	if writeErr := os.WriteFile(filepath.Join(repository, "dirty"), []byte("untracked"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	for _, cwd := range []string{repository, linked, nested} {
		got, resolveErr := ResolveProject(t.Context(), cwd, "", git)
		if resolveErr != nil || got != unborn {
			t.Fatalf("common identity for %s: %+v %v", cwd, got, resolveErr)
		}
	}
	independent := mkdir(t, filepath.Join(repository, "independent"))
	gitFixtureCommand(t, git, independent, "init", "-q")
	got, err := ResolveProject(t.Context(), independent, "", git)
	if err != nil || got.Path != filepath.Join(independent, ".git") || got == unborn {
		t.Fatalf("nested independent repository: %+v %v", got, err)
	}
	if _, err := ResolveProject(t.Context(), repository, repository, git); !errors.Is(err, ErrProjectContext) {
		t.Fatalf("non-Git project-root accepted in Git: %v", err)
	}
}

func TestResolveProjectNonGitCanonicalContainment(t *testing.T) {
	git := fixtureGit(t)
	root := canonicalTempDirectory(t)
	project := mkdir(t, filepath.Join(root, "project"))
	cwd := mkdir(t, filepath.Join(project, "nested"))
	want := task.TeamProject{Kind: "directory", Path: project}
	if got, err := ResolveProject(t.Context(), cwd, project, git); err != nil || got != want {
		t.Fatalf("non-Git root: %+v %v", got, err)
	}
	if got, err := ResolveProject(t.Context(), project, project, git); err != nil || got != want {
		t.Fatalf("cwd equals root: %+v %v", got, err)
	}
	if _, err := ResolveProject(t.Context(), cwd, "", git); !errors.Is(err, ErrProjectContext) {
		t.Fatalf("guessed non-Git root: %v", err)
	}
	sibling := mkdir(t, filepath.Join(root, "project-other"))
	if _, err := ResolveProject(t.Context(), sibling, project, git); !errors.Is(err, ErrProjectMismatch) {
		t.Fatalf("prefix sibling accepted: %v", err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveProject(t.Context(), filepath.Join(alias, "nested"), alias, git); err != nil || got != want {
		t.Fatalf("canonical symlink identity: %+v %v", got, err)
	}
	escape := filepath.Join(project, "escape")
	if err := os.Symlink(sibling, escape); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveProject(t.Context(), escape, project, git); !errors.Is(err, ErrProjectMismatch) {
		t.Fatalf("symlink escape accepted: %v", err)
	}
}

func TestResolveProjectNeverTreatsGitFailureAsAbsence(t *testing.T) {
	git := fixtureGit(t)
	directory := t.TempDir()
	git.Executable = filepath.Join(directory, "missing-git")
	if _, err := ResolveProject(t.Context(), directory, directory, git); !errors.Is(err, ErrProjectContext) || errors.Is(err, workspace.ErrNotGitRepository) {
		t.Fatalf("unavailable Git treated as non-Git: %v", err)
	}
	for _, cwd := range []string{"", "relative", filepath.Join(directory, "missing")} {
		if _, err := ResolveProject(t.Context(), cwd, directory, git); !errors.Is(err, ErrProjectContext) {
			t.Fatalf("invalid cwd accepted: %q: %v", cwd, err)
		}
	}
}

func TestMovedRepositoryDoesNotReassignStoredTeams(t *testing.T) {
	git := fixtureGit(t)
	root := canonicalTempDirectory(t)
	repository := mkdir(t, filepath.Join(root, "original"))
	gitFixtureCommand(t, git, repository, "init", "-q")
	original, err := ResolveProject(t.Context(), repository, "", git)
	if err != nil {
		t.Fatal(err)
	}
	catalog := testCatalog(t)
	ref := task.TeamReference{Scope: "project", Project: &original, TeamID: "review"}
	registerTeam(t, catalog, ref)
	movedPath := filepath.Join(root, "moved")
	if moveErr := os.Rename(repository, movedPath); moveErr != nil {
		t.Fatal(moveErr)
	}
	moved, err := ResolveProject(t.Context(), movedPath, "", git)
	if err != nil || moved.Path != filepath.Join(movedPath, ".git") || moved == original {
		t.Fatalf("moved project identity: %+v %v", moved, err)
	}
	listing, err := catalog.List(t.Context(), &moved)
	if err != nil || len(listing.Records) != 0 {
		t.Fatalf("old Teams silently reassigned: %+v %v", listing, err)
	}
	if record, _, lookupErr := catalog.Lookup(t.Context(), ref); lookupErr != nil || record.Reference.Project.Path != original.Path {
		t.Fatalf("old definitions were removed: %+v %v", record, lookupErr)
	}
}
