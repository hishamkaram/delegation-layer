package workspace

import "github.com/hishamkaram/delegation-layer/internal/workspace/internal/git"

// Git is the sole managed Git command boundary. It retains its existing API.
type Git = git.Git

type CommandRunner = git.CommandRunner

func ValidateGitEnvironment() error { return git.ValidateGitEnvironment() }
