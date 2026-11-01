package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

// ResolveRepositoryIdentity observes only working-tree and metadata paths. It
// requires neither a commit nor a clean checkout and never provisions state.
// Git owns discovery; the existing runner and path resolver own execution and
// canonicalization. Only a verified discovery refusal is ErrNotGitRepository.
func (g Git) ResolveRepositoryIdentity(ctx context.Context, cwd string) (contract.RepositoryIdentity, error) {
	if ctx == nil {
		return contract.RepositoryIdentity{}, fmt.Errorf("%w: nil repository identity context", contract.ErrInvalidGitContext)
	}
	directory, err := existingDirectory(cwd)
	if err != nil {
		return contract.RepositoryIdentity{}, err
	}
	stdout, stderr, err := g.runOutput(ctx, directory, []string{"rev-parse", "--show-toplevel"}, map[string]string{"LC_ALL": "C"}, nil)
	if err != nil {
		cause := gitCommandError(err, stderr)
		if len(stdout) == 0 && gitExitedOnlyWith(err, gitFatalExit) && nonRepositoryDiagnostic(string(stderr)) {
			cause = errors.Join(contract.ErrNotGitRepository, cause)
		}
		return contract.RepositoryIdentity{}, fmt.Errorf("read Git root: %w", cause)
	}
	rootPath := removeOneTerminatingNewline(string(stdout))
	if !filepath.IsAbs(rootPath) {
		return contract.RepositoryIdentity{}, fmt.Errorf("%w: Git root is not absolute", contract.ErrInvalidGitContext)
	}
	root, err := existingDirectory(rootPath)
	if err != nil {
		return contract.RepositoryIdentity{}, fmt.Errorf("resolve Git root: %w", err)
	}
	gitDir, err := g.resolveIdentityDirectory(ctx, directory, "--git-dir")
	if err != nil {
		return contract.RepositoryIdentity{}, err
	}
	commonDir, err := g.resolveIdentityDirectory(ctx, directory, "--git-common-dir")
	if err != nil {
		return contract.RepositoryIdentity{}, err
	}
	return contract.RepositoryIdentity{Root: root, GitDir: gitDir, CommonDir: commonDir}, nil
}

func (g Git) resolveIdentityDirectory(ctx context.Context, directory, option string) (string, error) {
	value, err := g.runScalar(ctx, directory, []string{"rev-parse", option}, nil)
	if err != nil {
		return "", fmt.Errorf("read Git %s: %w", option, err)
	}
	if value == "" {
		return "", fmt.Errorf("%w: Git %s is empty", contract.ErrInvalidGitContext, option)
	}
	resolved, err := resolveGitDirectory(value, directory)
	if err != nil {
		return "", fmt.Errorf("resolve Git %s: %w", option, err)
	}
	return resolved, nil
}

// Git has no distinct non-repository exit code. Pin the discovery locale and
// accept only its two exact discovery refusals, with no independent fault.
// https://github.com/git/git/blob/master/setup.c
func nonRepositoryDiagnostic(diagnostic string) bool {
	if diagnostic == "fatal: not a git repository (or any of the parent directories): .git\n" {
		return true
	}
	mount, found := strings.CutPrefix(diagnostic, "fatal: not a git repository (or any parent up to mount point ")
	if !found {
		return false
	}
	mount, found = strings.CutSuffix(mount, ")\nStopping at filesystem boundary (GIT_DISCOVERY_ACROSS_FILESYSTEM not set).\n")
	return found && filepath.IsAbs(mount)
}
