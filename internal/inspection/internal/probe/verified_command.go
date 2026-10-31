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

// newVerifiedCommand binds an integrity-bound native helper to its admitted
// bytes. It is never used for the installed provider CLI.
func newVerifiedCommand(scope commandBinder, executable, digest, directory string, environment []string, args ...string) (*exec.Cmd, *verifiedCommandFiles, error) {
	verified, err := scope.BindCommand(verifiedexec.CommandRequest{
		Executable: executable, SHA256: digest, Directory: directory, Environment: environment, Args: args,
	})
	if err != nil {
		return nil, nil, err
	}
	return verified.Cmd, verified, nil
}

// newProviderCommand builds one unstarted command for the installed provider
// CLI at its resolved absolute path. The OS performs any shebang handling; no
// copy, view, or byte verification is made.
func newProviderCommand(executable, directory string, environment []string, args ...string) *exec.Cmd {
	cmd := exec.Command(executable, args...)
	cmd.Dir, cmd.Env = directory, environment
	return cmd
}
