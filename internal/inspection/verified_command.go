package inspection

import (
	"os/exec"

	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// verifiedCommandFiles keeps the inspection package's existing ownership
// shape while sharing the launch binding with ordinary provider execution.
type verifiedCommandFiles = verifiedexec.Command

func newVerifiedCommand(executable, digest, directory string, environment []string, args ...string) (*exec.Cmd, *verifiedCommandFiles, error) {
	verified, err := verifiedexec.NewCommandInDirectory(executable, digest, directory, environment, args...)
	if err != nil {
		return nil, nil, err
	}
	return verified.Cmd, verified, nil
}
