package teams

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// ResolveProject uses Git's canonical common metadata directory as repository
// identity. Verified non-Git directories require an explicit containing root;
// no process cwd, remote, branch or guessed marker participates in selection.
func ResolveProject(ctx context.Context, cwd, projectRoot string, git workspace.Git) (task.TeamProject, error) {
	if err := ctx.Err(); err != nil {
		return task.TeamProject{}, fmt.Errorf("%w: %w", ErrProjectContext, err)
	}
	directory, err := canonicalDirectory(cwd)
	if err != nil {
		return task.TeamProject{}, err
	}
	identity, err := git.ResolveRepositoryIdentity(ctx, directory)
	if err == nil {
		if projectRoot != "" {
			return task.TeamProject{}, fmt.Errorf("%w: project-root is only valid outside Git", ErrProjectContext)
		}
		return task.TeamProject{Kind: "git", Path: identity.CommonDir}, nil
	}
	if !errors.Is(err, workspace.ErrNotGitRepository) {
		return task.TeamProject{}, fmt.Errorf("%w: observing Git identity: %w", ErrProjectContext, err)
	}
	if projectRoot == "" {
		return task.TeamProject{}, fmt.Errorf("%w: outside Git, supply an absolute project-root containing cwd", ErrProjectContext)
	}
	root, err := canonicalDirectory(projectRoot)
	if err != nil {
		return task.TeamProject{}, err
	}
	relative, err := filepath.Rel(root, directory)
	if err != nil {
		return task.TeamProject{}, fmt.Errorf("%w: comparing project-root and cwd: %w", ErrProjectContext, err)
	}
	if !filepath.IsLocal(relative) {
		return task.TeamProject{}, fmt.Errorf("%w: cwd must be contained in project-root", ErrProjectMismatch)
	}
	return task.TeamProject{Kind: "directory", Path: root}, nil
}

func canonicalDirectory(path string) (string, error) {
	if !task.IsCanonicalCwd(path) {
		return "", fmt.Errorf("%w: directory must be an explicit absolute, clean path", ErrProjectContext)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%w: resolving directory: %w", ErrProjectContext, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: inspecting directory: %w", ErrProjectContext, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: expected an existing directory", ErrProjectContext)
	}
	return canonical, nil
}
