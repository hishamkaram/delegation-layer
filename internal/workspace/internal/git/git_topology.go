package git

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

// The common Git directory is the shared topology owner across state roots.
// This leaf lease covers one native worktree command, not a provider turn or
// capture. The inherited descriptor keeps it held if the helper exits first.
func (g Git) withWorktreeMetadata(ctx context.Context, source contract.Source, run func(Git) error) (resultErr error) {
	if ctx == nil {
		return errors.Join(contract.ErrInvalidGitContext, errors.New("nil worktree command context"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.Runner == nil {
		return errGitRunnerRequired
	}
	lock, err := taskdir.OpenDirectoryLock(source.CommonDir, taskdir.LockLevelRun)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	if err = lock.LockEX(ctx); err != nil {
		return err
	}
	file, err := lock.TakeExclusiveFile()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	bound := g
	bound.Runner = worktreeMetadataRunner{runner: g.Runner, file: file}
	// The bound runner is borrowed only for this synchronous callback.
	return run(bound)
}

func (g Git) runWorktreeCommand(ctx context.Context, source contract.Source, args []string) error {
	return g.withWorktreeMetadata(ctx, source, func(bound Git) error {
		return bound.run(ctx, source.Root, args, nil, nil, nil, nil)
	})
}

type worktreeMetadataRunner struct {
	runner CommandRunner
	file   *os.File
}

func (r worktreeMetadataRunner) Run(ctx context.Context, command *exec.Cmd) error {
	command.ExtraFiles = append(command.ExtraFiles, r.file)
	return r.runner.Run(ctx, command)
}
