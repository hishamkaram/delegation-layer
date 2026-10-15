package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	lifetimeLaunchGateMode = "--lifetime-launch-gate"
	lifetimeLaunchGateFD   = "--gate-fd"
	lifetimeLaunchToken    = byte(1)
)

type lifetimeCommandLaunchGate interface {
	started(error) error
	release() error
	abort() error
}

type lifetimeCommandLaunchGateFactory func(*exec.Cmd) (lifetimeCommandLaunchGate, error)

type runnerLifetimeLaunchGate struct {
	reader *os.File
	writer *os.File
}

// newRunnerLifetimeLaunchGateFactory wraps a provider in the current managed
// runner. The wrapper blocks inside the configured process scope until the
// guardian has durably recorded that exact PID, then execs the provider in the
// same process. Execution remains the sole owner of cmd.Wait.
func newRunnerLifetimeLaunchGateFactory(runner string) lifetimeCommandLaunchGateFactory {
	return func(cmd *exec.Cmd) (lifetimeCommandLaunchGate, error) {
		if cmd == nil || cmd.Process != nil || cmd.ProcessState != nil {
			return nil, task.ErrEvidenceFault
		}
		if !filepath.IsAbs(runner) || filepath.Clean(runner) != runner {
			return nil, task.ErrEvidenceFault
		}
		providerPath := cmd.Path
		if !filepath.IsAbs(providerPath) || filepath.Clean(providerPath) != providerPath {
			return nil, task.ErrEvidenceFault
		}
		providerArgs := slices.Clone(cmd.Args)
		if len(providerArgs) == 0 {
			providerArgs = []string{providerPath}
		}
		if providerArgs[0] == "" {
			return nil, task.ErrEvidenceFault
		}

		reader, writer, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("create lifetime launch gate: %w", err)
		}
		gate := &runnerLifetimeLaunchGate{reader: reader, writer: writer}
		gateFD := 3 + len(cmd.ExtraFiles)
		cmd.ExtraFiles = append(cmd.ExtraFiles, reader)
		cmd.Path = runner
		cmd.Args = []string{
			runner,
			lifetimeLaunchGateMode,
			lifetimeLaunchGateFD,
			strconv.Itoa(gateFD),
			"--",
			providerPath,
		}
		cmd.Args = append(cmd.Args, providerArgs...)
		return gate, nil
	}
}

func (g *runnerLifetimeLaunchGate) started(startErr error) error {
	if g == nil {
		return task.ErrEvidenceFault
	}
	closeErr := closeLifetimeLaunchGateFile(&g.reader)
	if startErr != nil {
		closeErr = errors.Join(closeErr, closeLifetimeLaunchGateFile(&g.writer))
	}
	return closeErr
}

func (g *runnerLifetimeLaunchGate) release() error {
	if g == nil || g.writer == nil {
		return task.ErrEvidenceFault
	}
	_, writeErr := g.writer.Write([]byte{lifetimeLaunchToken})
	closeErr := closeLifetimeLaunchGateFile(&g.writer)
	return errors.Join(writeErr, closeErr)
}

func (g *runnerLifetimeLaunchGate) abort() error {
	if g == nil {
		return nil
	}
	return errors.Join(closeLifetimeLaunchGateFile(&g.reader), closeLifetimeLaunchGateFile(&g.writer))
}

func closeLifetimeLaunchGateFile(file **os.File) error {
	if file == nil || *file == nil {
		return nil
	}
	current := *file
	*file = nil
	return current.Close()
}

func runLifetimeLaunchGateMode(args []string, stderr io.Writer) (int, bool) {
	if len(args) == 0 || args[0] != lifetimeLaunchGateMode {
		return 0, false
	}
	if err := runLifetimeLaunchGate(args[1:], syscall.Exec); err != nil {
		return writeCLIError(stderr, errors.New("task lifetime launch unavailable"), 1), true
	}
	return 0, true
}

func runLifetimeLaunchGate(args []string, execProcess func(string, []string, []string) error) error {
	gateFD, providerPath, providerArgs, err := parseLifetimeLaunchGateArguments(args)
	if err != nil || execProcess == nil {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	gate := os.NewFile(uintptr(gateFD), "delegate-provider-launch-gate")
	if gate == nil {
		return task.ErrEvidenceFault
	}
	var token [1]byte
	_, readErr := io.ReadFull(gate, token[:])
	closeErr := gate.Close()
	if readErr != nil || closeErr != nil || token[0] != lifetimeLaunchToken {
		return errors.Join(task.ErrEvidenceFault, readErr, closeErr)
	}
	return execProcess(providerPath, providerArgs, os.Environ())
}

func parseLifetimeLaunchGateArguments(args []string) (int, string, []string, error) {
	if len(args) < 5 || args[0] != lifetimeLaunchGateFD || args[2] != "--" {
		return 0, "", nil, task.ErrEvidenceFault
	}
	gateFD, err := strconv.Atoi(args[1])
	if err != nil || gateFD < 3 {
		return 0, "", nil, task.ErrEvidenceFault
	}
	providerPath := args[3]
	providerArgs := slices.Clone(args[4:])
	if !filepath.IsAbs(providerPath) || filepath.Clean(providerPath) != providerPath || len(providerArgs) == 0 || providerArgs[0] == "" {
		return 0, "", nil, task.ErrEvidenceFault
	}
	return gateFD, providerPath, providerArgs, nil
}
