package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacescore "github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func verifyManagedCheckout(scope execution.PreflightScope, store *taskdir.Store, resource *workspace.Resource, request workspace.OperationRequest) (exit *int, artifacts []workspace.Artifact, resultErr error) {
	if len(request.Arguments) == 0 {
		return nil, nil, task.ErrIdentityMismatch
	}
	checkout, err := workspacescore.RegisteredExecutionCheckout(scope.Context(), resource)
	if err != nil {
		return nil, nil, err
	}
	scratch := workspace.ScratchPath(store.Root, request.OperationID)
	if err = os.MkdirAll(scratch, 0o700); err != nil {
		return nil, nil, err
	}
	stdout, err := newWorkspaceDiagnostic(scratch, "stdout")
	if err != nil {
		return nil, nil, err
	}
	stderr, err := newWorkspaceDiagnostic(scratch, "stderr")
	if err != nil {
		return nil, nil, errors.Join(err, stdout.Close())
	}
	command := exec.Command(request.Arguments[0], request.Arguments[1:]...)
	command.Dir = checkout.Workdir
	command.Env = command.Environ()
	command.Stdout, command.Stderr = stdout, stderr
	commandErr := execution.RunScopedCommand(scope, command, execution.CommandHooks{})
	closeErr := errors.Join(stdout.Close(), stderr.Close())
	code := 0
	if commandErr != nil {
		exitError, exitErr := workspaceCommandExit(commandErr)
		if exitErr != nil {
			return nil, nil, errors.Join(exitErr, closeErr)
		}
		if exitError == nil {
			return nil, nil, errors.Join(commandErr, closeErr)
		}
		code = exitError.ExitCode()
	}
	if closeErr != nil {
		return nil, nil, closeErr
	}
	return &code, []workspace.Artifact{stdout.Artifact(), stderr.Artifact()}, nil
}

type workspaceJoinedError interface {
	Unwrap() []error
}

func workspaceCommandExit(commandErr error) (*exec.ExitError, error) {
	var exitErr *exec.ExitError
	if !errors.As(commandErr, &exitErr) {
		return nil, commandErr
	}
	var combined workspaceJoinedError
	if errors.As(commandErr, &combined) {
		return nil, commandErr
	}
	return exitErr, nil
}

// workspaceDiagnostic keeps a bounded prefix while consuming every byte. Its
// descriptor explicitly distinguishes truncation from complete command output.
type workspaceDiagnostic struct {
	file      *os.File
	name      string
	digest    hash.Hash
	length    int64
	truncated bool
	fault     error
}

func newWorkspaceDiagnostic(directory, name string) (*workspaceDiagnostic, error) {
	file, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &workspaceDiagnostic{file: file, name: name, digest: sha256.New()}, nil
}

func (w *workspaceDiagnostic) Write(data []byte) (int, error) {
	if w.fault != nil {
		return len(data), w.fault
	}
	keep := min(int64(len(data)), workspace.ArtifactLimit-w.length)
	if keep < int64(len(data)) {
		w.truncated = true
	}
	if keep != 0 {
		written, err := w.file.Write(data[:keep])
		if written > 0 {
			_, digestErr := w.digest.Write(data[:written])
			w.length += int64(written)
			err = errors.Join(err, digestErr)
		}
		if err != nil {
			w.fault = err
			return len(data), err
		}
	}
	return len(data), nil
}

func (w *workspaceDiagnostic) Close() error {
	return errors.Join(w.fault, w.file.Sync(), w.file.Close())
}

func (w *workspaceDiagnostic) Artifact() workspace.Artifact {
	return workspace.Artifact{Name: w.name, Length: w.length, SHA256: hex.EncodeToString(w.digest.Sum(nil)), Truncated: w.truncated}
}
