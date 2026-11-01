package git

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

func TestResolveRepositoryIdentityClassifiesOnlyVerifiedDiscoveryRefusals(t *testing.T) {
	const notRepository = "fatal: not a git repository (or any of the parent directories): .git\n"
	fault := errors.New("runner stream close failed")
	tests := []struct {
		name       string
		stdout     string
		stderr     string
		runErr     error
		notGitRepo bool
	}{
		{name: "verified", stderr: notRepository, runErr: gitTestExitError(128), notGitRepo: true},
		{name: "filesystem boundary", stderr: "fatal: not a git repository (or any parent up to mount point " + filepath.VolumeName(os.TempDir()) + string(filepath.Separator) + ")\nStopping at filesystem boundary (GIT_DISCOVERY_ACROSS_FILESYSTEM not set).\n", runErr: gitTestExitError(128), notGitRepo: true},
		{name: "exit alone", runErr: gitTestExitError(128)},
		{name: "wrong exit", stderr: notRepository, runErr: gitTestExitError(1)},
		{name: "runner fault", stderr: notRepository, runErr: fault},
		{name: "joined fault", stderr: notRepository, runErr: errors.Join(gitTestExitError(128), fault)},
		{name: "joined cancellation", stderr: notRepository, runErr: errors.Join(gitTestExitError(128), context.Canceled)},
		{name: "unexpected stdout", stdout: "partial", stderr: notRepository, runErr: gitTestExitError(128)},
		{name: "additional warning", stderr: "warning: config failure\n" + notRepository, runErr: gitTestExitError(128)},
		{name: "additional fault", stderr: notRepository + "fatal: config failure\n", runErr: gitTestExitError(128)},
		{name: "bad config", stderr: "fatal: bad config line 1 in file config\n", runErr: gitTestExitError(128)},
		{name: "dubious ownership", stderr: "fatal: detected dubious ownership in repository\n", runErr: gitTestExitError(128)},
		{name: "bare repository", stderr: "fatal: this operation must be run in a work tree\n", runErr: gitTestExitError(128)},
		{name: "broken gitfile", stderr: "fatal: not a git repository: missing.git\n", runErr: gitTestExitError(128)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingGitRunner{handler: func(command *exec.Cmd) error {
				if countEnvironmentEntry(command.Env, "LC_ALL=C") != 1 {
					t.Error("discovery locale was not pinned")
				}
				_, stdoutErr := io.WriteString(command.Stdout, test.stdout)
				_, stderrErr := io.WriteString(command.Stderr, test.stderr)
				return errors.Join(test.runErr, stdoutErr, stderrErr)
			}}
			identity, err := (Git{Runner: runner, Executable: os.Args[0]}).ResolveRepositoryIdentity(t.Context(), canonicalWorkspaceTestDir(t))
			if identity != (contract.RepositoryIdentity{}) || !errors.Is(err, test.runErr) {
				t.Fatalf("identity = %+v, error = %v; want zero identity and original cause", identity, err)
			}
			if errors.Is(err, contract.ErrNotGitRepository) != test.notGitRepo {
				t.Fatalf("error = %v; not a repository = %t, want %t", err, errors.Is(err, contract.ErrNotGitRepository), test.notGitRepo)
			}
			if len(runner.calls) != 1 {
				t.Fatalf("command count = %d, want discovery only", len(runner.calls))
			}
		})
	}
}

func TestResolveRepositoryIdentityKeepsLaterGitFailures(t *testing.T) {
	for _, option := range []string{"--git-dir", "--git-common-dir"} {
		t.Run(option, func(t *testing.T) {
			root := canonicalWorkspaceTestDir(t)
			gitDir := filepath.Join(root, ".git")
			if err := os.Mkdir(gitDir, 0o700); err != nil {
				t.Fatal(err)
			}
			runner := &recordingGitRunner{handler: func(command *exec.Cmd) error {
				if command.Args[len(command.Args)-1] == option {
					_, writeErr := io.WriteString(command.Stderr, "fatal: not a git repository (or any of the parent directories): .git\n")
					return errors.Join(gitTestExitError(128), writeErr)
				}
				value := root
				if command.Args[len(command.Args)-1] == "--git-dir" {
					value = gitDir
				}
				return writeCommandOutput(command, value+"\n")
			}}
			identity, err := (Git{Runner: runner, Executable: os.Args[0]}).ResolveRepositoryIdentity(t.Context(), root)
			if identity != (contract.RepositoryIdentity{}) || !errors.Is(err, gitTestExitError(128)) || errors.Is(err, contract.ErrNotGitRepository) {
				t.Fatalf("identity = %+v, error = %v; later discovery failure must remain a fault", identity, err)
			}
		})
	}
}

func TestResolveRepositoryIdentityRejectsMalformedPathOutput(t *testing.T) {
	tests := []struct {
		option string
		value  string
	}{
		{option: "--show-toplevel", value: ""},
		{option: "--show-toplevel", value: "."},
		{option: "--git-dir", value: ""},
		{option: "--git-dir", value: "\x00"},
		{option: "--git-common-dir", value: ""},
		{option: "--git-common-dir", value: "\x00"},
	}
	for _, test := range tests {
		t.Run(test.option+"/"+strings.ReplaceAll(test.value, "\x00", "NUL"), func(t *testing.T) {
			root := canonicalWorkspaceTestDir(t)
			runner := &recordingGitRunner{handler: func(command *exec.Cmd) error {
				value := root
				if command.Args[len(command.Args)-1] == test.option {
					value = test.value
				}
				return writeCommandOutput(command, value+"\n")
			}}
			identity, err := (Git{Runner: runner, Executable: os.Args[0]}).ResolveRepositoryIdentity(t.Context(), root)
			if identity != (contract.RepositoryIdentity{}) || !errors.Is(err, contract.ErrInvalidGitContext) || errors.Is(err, contract.ErrNotGitRepository) {
				t.Fatalf("identity = %+v, error = %v; want invalid path output", identity, err)
			}
		})
	}
}

func TestResolveRepositoryIdentityPreservesUnavailableGitAndInputErrors(t *testing.T) {
	root := canonicalWorkspaceTestDir(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runner := &recordingGitRunner{}
	tests := []struct {
		name string
		git  Git
		ctx  context.Context
		cwd  string
		want error
	}{
		{name: "unavailable Git", git: Git{Runner: runner, Environment: []string{"PATH=" + root}}, ctx: t.Context(), cwd: root, want: exec.ErrNotFound},
		{name: "missing runner", git: Git{}, ctx: t.Context(), cwd: root, want: errGitRunnerRequired},
		{name: "canceled", git: Git{Runner: runner}, ctx: ctx, cwd: root, want: context.Canceled},
		{name: "nil context", git: Git{Runner: runner}, cwd: root, want: contract.ErrInvalidGitContext},
		{name: "empty cwd", git: Git{Runner: runner}, ctx: t.Context(), want: contract.ErrInvalidGitContext},
		{name: "missing cwd", git: Git{Runner: runner}, ctx: t.Context(), cwd: filepath.Join(root, "missing"), want: os.ErrNotExist},
		{name: "repository redirection", git: Git{Runner: runner, Environment: []string{"GIT_DIR=" + root}}, ctx: t.Context(), cwd: root, want: contract.ErrInvalidGitContext},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity, err := test.git.ResolveRepositoryIdentity(test.ctx, test.cwd)
			if identity != (contract.RepositoryIdentity{}) || !errors.Is(err, test.want) || errors.Is(err, contract.ErrNotGitRepository) {
				t.Fatalf("identity = %+v, error = %v; want zero identity and %v", identity, err, test.want)
			}
		})
	}
	if len(runner.calls) != 0 {
		t.Fatalf("input failures launched %d commands", len(runner.calls))
	}
}
