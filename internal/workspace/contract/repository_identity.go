package contract

import "errors"

// RepositoryIdentity is a read-only observation of a working tree and its Git
// metadata. Paths are absolute, clean, native paths with symlinks resolved.
// Linked worktrees have distinct Root and GitDir values and share CommonDir;
// independent clones remain distinct even when they use the same remote.
type RepositoryIdentity struct {
	Root      string
	GitDir    string
	CommonDir string
}

// ErrNotGitRepository means native Git verified that directory discovery found
// no repository. Executable, configuration, runner and repository faults do not
// carry this marker. A bare repository has no working tree and is an error.
var ErrNotGitRepository = errors.New("directory is not in a Git repository")
