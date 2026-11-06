package host

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

type childProcess struct {
	identity Identity
	control  *channel
	wait     <-chan error
}

// startChild owns Start and creates exactly one Wait on success. A returned
// process must always be joined, including when observing its identity failed.
func startChild(config Config) (*childProcess, error) {
	selected, err := verifiedexec.LocatePath(config.Executable)
	if err != nil {
		return nil, err
	}
	if selected != config.Executable {
		return nil, ErrIdentity
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	eventRead, eventWrite, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, controlRead.Close(), controlWrite.Close())
	}
	args := append(slices.Clone(config.Args), ChildReadFDFlag+"=3", ChildWriteFDFlag+"=4")
	cmd := exec.Command(config.Executable, args...)
	cmd.Env, cmd.Dir = slices.Clone(config.Environment), config.Directory
	cmd.ExtraFiles = []*os.File{controlRead, eventWrite}
	if err = lifetime.ConfigureIndependentSession(cmd); err == nil {
		err = cmd.Start()
	}
	parentCloseErr := errors.Join(controlRead.Close(), eventWrite.Close())
	if err != nil {
		return nil, errors.Join(err, parentCloseErr, controlWrite.Close(), eventRead.Close())
	}
	ref, identityErr := lifetime.ObserveStartedProcess(cmd.Process.Pid)
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	child := &childProcess{identity: identity(ref), control: newChannel(eventRead, controlWrite), wait: wait}
	if identityErr != nil {
		return child, errors.Join(fmt.Errorf("observe started scheduler child: %w", identityErr), parentCloseErr)
	}
	return child, parentCloseErr
}

func busyExit(err error) bool {
	var exited *exec.ExitError
	return errors.As(err, &exited) && exited.ExitCode() == ChildBusyExitCode
}
