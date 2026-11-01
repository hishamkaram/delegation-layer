package git_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestRepositoryIdentityUnbornAndDirtyThroughFacade(t *testing.T) {
	base, git, runner := newNativeIdentityGit(t)
	root := filepath.Join(base, "unborn repo\nwith trailing space ")
	initIdentityRepository(t, git, root)
	want := workspace.RepositoryIdentity{Root: root, GitDir: filepath.Join(root, ".git"), CommonDir: filepath.Join(root, ".git")}
	assertReadOnlyIdentity(t, git, root, want)
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"staged", "untracked"} {
		if err := os.WriteFile(filepath.Join(nested, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runIdentityFixtureGit(t, git, root, "add", "nested/staged")
	assertReadOnlyIdentity(t, git, nested, want)
	assertIdentityMetadataReads(t, runner.calls)
}

func TestRepositoryIdentityLinkedWorktreeSharesOnlyCommonDirectory(t *testing.T) {
	base, git, runner := newNativeIdentityGit(t)
	root := filepath.Join(base, "primary")
	initIdentityRepository(t, git, root)
	commitIdentityRepository(t, git, root)
	linked := filepath.Join(base, "linked")
	runIdentityFixtureGit(t, git, root, "worktree", "add", "--detach", linked, "HEAD")
	nested := filepath.Join(linked, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "dirty"), []byte("untracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(root, ".git")
	assertReadOnlyIdentity(t, git, root, workspace.RepositoryIdentity{Root: root, GitDir: common, CommonDir: common})
	assertReadOnlyIdentity(t, git, nested, workspace.RepositoryIdentity{Root: linked, GitDir: filepath.Join(common, "worktrees", "linked"), CommonDir: common})
	assertIdentityMetadataReads(t, runner.calls)
}

func TestRepositoryIdentitySeparateClonesWithSameRemoteStayDistinct(t *testing.T) {
	base, git, runner := newNativeIdentityGit(t)
	origin := filepath.Join(base, "origin")
	initIdentityRepository(t, git, origin)
	commitIdentityRepository(t, git, origin)
	for _, name := range []string{"first", "second"} {
		root := filepath.Join(base, name)
		runIdentityFixtureGit(t, git, base, "clone", "--quiet", "--no-hardlinks", origin, root)
		gitDir := filepath.Join(root, ".git")
		assertReadOnlyIdentity(t, git, root, workspace.RepositoryIdentity{Root: root, GitDir: gitDir, CommonDir: gitDir})
	}
	assertIdentityMetadataReads(t, runner.calls)
}

func TestRepositoryIdentityCanonicalizesSymlinkAliases(t *testing.T) {
	base, git, runner := newNativeIdentityGit(t)
	root := filepath.Join(base, "repository")
	initIdentityRepository(t, git, root)
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(base, "metadata")
	if err := os.Rename(filepath.Join(root, ".git"), gitDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gitDir, filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	want := workspace.RepositoryIdentity{Root: root, GitDir: gitDir, CommonDir: gitDir}
	assertReadOnlyIdentity(t, git, filepath.Join(alias, "nested"), want)
	t.Chdir(base)
	assertReadOnlyIdentity(t, git, filepath.Join("alias", "nested"), want)
	assertIdentityMetadataReads(t, runner.calls)
}

func TestRepositoryIdentityNativeNonGitDirectory(t *testing.T) {
	base, git, runner := newNativeIdentityGit(t)
	root := filepath.Join(base, "non-git")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	before := identityTreeSnapshot(t, root)
	identity, err := git.ResolveRepositoryIdentity(t.Context(), root)
	if identity != (workspace.RepositoryIdentity{}) || !errors.Is(err, workspace.ErrNotGitRepository) {
		t.Fatalf("identity = %+v, error = %v; want verified non-Git directory", identity, err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 128 {
		t.Fatalf("error = %v; native exit cause was lost", err)
	}
	assertIdentityTreeUnchanged(t, root, before)
	assertIdentityMetadataReads(t, runner.calls)
}

func TestRepositoryIdentityNativeRepositoryFaults(t *testing.T) {
	for _, name := range []string{"global config", "repository config", "broken gitfile", "bare repository"} {
		t.Run(name, func(t *testing.T) {
			base, git, runner := newNativeIdentityGit(t)
			root := filepath.Join(base, "repository")
			initIdentityRepository(t, git, root)
			switch name {
			case "global config":
				writeIdentityFault(t, filepath.Join(base, "global-config"), "[invalid\n")
			case "repository config":
				writeIdentityFault(t, filepath.Join(root, ".git", "config"), "[invalid\n")
			case "broken gitfile":
				root = filepath.Join(base, "broken")
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				writeIdentityFault(t, filepath.Join(root, ".git"), "gitdir: "+filepath.Join(base, "missing.git")+"\n")
			case "bare repository":
				root = filepath.Join(base, "bare.git")
				runIdentityFixtureGit(t, git, base, "init", "--quiet", "--bare", "--template=", root)
			}
			before := identityTreeSnapshot(t, root)
			identity, err := git.ResolveRepositoryIdentity(t.Context(), root)
			if identity != (workspace.RepositoryIdentity{}) || err == nil || errors.Is(err, workspace.ErrNotGitRepository) {
				t.Fatalf("identity = %+v, error = %v; repository fault must remain an error", identity, err)
			}
			assertIdentityTreeUnchanged(t, root, before)
			assertIdentityMetadataReads(t, runner.calls)
		})
	}
}

type nativeIdentityRunner struct {
	calls [][]string
}

func (r *nativeIdentityRunner) Run(ctx context.Context, command *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.calls = append(r.calls, slices.Clone(command.Args[3:]))
	return command.Run()
}

func newNativeIdentityGit(t *testing.T) (string, workspace.Git, *nativeIdentityRunner) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	runner := &nativeIdentityRunner{}
	git := workspace.Git{Runner: runner, Executable: executable, Environment: []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(base, "global-config"),
		"GIT_CEILING_DIRECTORIES=" + base,
		"LC_ALL=C",
	}}
	return base, git, runner
}

func initIdentityRepository(t *testing.T, git workspace.Git, root string) {
	t.Helper()
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	runIdentityFixtureGit(t, git, root, "init", "--quiet", "--template=", "--initial-branch=main", "--object-format=sha1")
}

func commitIdentityRepository(t *testing.T, git workspace.Git, root string) {
	t.Helper()
	runIdentityFixtureGit(t, git, root, "-c", "user.name=Identity Test", "-c", "user.email=identity@example.invalid", "-c", "commit.gpgsign=false", "commit", "--quiet", "--allow-empty", "-m", "fixture")
}

func runIdentityFixtureGit(t *testing.T, git workspace.Git, cwd string, args ...string) {
	t.Helper()
	command := exec.Command(git.Executable, append([]string{"-C", cwd}, args...)...)
	command.Dir = cwd
	command.Env = slices.Clone(git.Environment)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fixture git %v: %v: %s", args, err, output)
	}
}

func assertReadOnlyIdentity(t *testing.T, git workspace.Git, cwd string, want workspace.RepositoryIdentity) {
	t.Helper()
	before := identityTreeSnapshot(t, want.Root)
	metadataBefore := identityTreeSnapshot(t, want.CommonDir)
	got, err := git.ResolveRepositoryIdentity(t.Context(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
	for _, path := range []string{got.Root, got.GitDir, got.CommonDir} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			t.Fatalf("path %q must be absolute and clean", path)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path {
			t.Fatalf("path %q is not canonical: %q, %v", path, resolved, err)
		}
	}
	assertIdentityTreeUnchanged(t, want.Root, before)
	assertIdentityTreeUnchanged(t, want.CommonDir, metadataBefore)
}

func assertIdentityMetadataReads(t *testing.T, calls [][]string) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("identity resolver did not query native Git")
	}
	for _, args := range calls {
		if len(args) != 2 || args[0] != "rev-parse" || !slices.Contains([]string{"--show-toplevel", "--git-dir", "--git-common-dir"}, args[1]) {
			t.Fatalf("identity resolution issued non-metadata command %v", args)
		}
	}
}

func identityTreeSnapshot(t *testing.T, root string) map[string][sha256.Size]byte {
	t.Helper()
	snapshot := make(map[string][sha256.Size]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		var data []byte
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			data = []byte(target)
		} else if !info.IsDir() {
			var err error
			data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		snapshot[path] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertIdentityTreeUnchanged(t *testing.T, root string, before map[string][sha256.Size]byte) {
	t.Helper()
	after := identityTreeSnapshot(t, root)
	if len(before) != len(after) {
		t.Fatalf("identity resolution changed tree entry count from %d to %d", len(before), len(after))
	}
	for path, digest := range before {
		if got, found := after[path]; !found || got != digest {
			t.Fatalf("identity resolution changed %s", path)
		}
	}
}

func writeIdentityFault(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}
