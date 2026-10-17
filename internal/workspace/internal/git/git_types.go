// Package workspace owns the Git observations and worktree primitives used by
// managed workspaces. It never starts or waits for a subprocess; callers inject
// the already-supervised command runner that owns those operations.
package git

import (
	"context"
	"os/exec"
)

const defaultGitExecutable = "git"

// CommandRunner runs one fully constructed Git command inside an existing
// supervised lifetime. Implementations own Start, Wait, stream draining, and
// deadline handling. Git methods only populate exec.Cmd and call Run.
type CommandRunner interface {
	Run(context.Context, *exec.Cmd) error
}

// Git is the managed Git command boundary. Runner is required; Executable is
// optional and defaults to git, allowing the application to bind a verified
// executable without changing command construction.
type Git struct {
	Runner     CommandRunner
	Executable string
}
