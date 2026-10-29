package probe

import (
	"os/exec"

	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// verifiedCommandFiles keeps the inspection package's existing ownership
// shape while sharing the launch binding with ordinary provider execution.
type verifiedCommandFiles = verifiedexec.Command

// commandBinder is the binding half of the supervised inspection scope. The
// scope owns the binding decision; execution.PreflightScope satisfies it.
type commandBinder interface {
	BindCommand(verifiedexec.CommandRequest) (*verifiedexec.Command, error)
}

func newVerifiedCommand(scope commandBinder, executable, digest, directory string, environment []string, args ...string) (*exec.Cmd, *verifiedCommandFiles, error) {
	verified, err := scope.BindCommand(verifiedexec.CommandRequest{
		Executable: executable, SHA256: digest, Directory: directory, Environment: environment, Args: args,
	})
	if err != nil {
		return nil, nil, err
	}
	return verified.Cmd, verified, nil
}
