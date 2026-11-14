package gatewayservice

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

const maxCommandOutput = 65536

// NativeRunner reuses Native's joined one-shot command owner. The selected
// installed Native executable must support its ordinary internal helper entry.
type NativeRunner struct {
	ownerExecutable string
	environment     []string
}

func NewNativeRunner(ownerExecutable string, environment []string) (*NativeRunner, error) {
	selected, err := verifiedexec.LocatePath(ownerExecutable)
	if err != nil || selected != ownerExecutable {
		return nil, errors.Join(ErrConfiguration, err)
	}
	return &NativeRunner{ownerExecutable: ownerExecutable, environment: slices.Clone(environment)}, nil
}

func (r NativeRunner) Run(ctx context.Context, executable string, args []string) (Result, error) {
	if err := finite(ctx); err != nil {
		return Result{}, err
	}
	selected, err := verifiedexec.LocatePath(executable)
	if err != nil || selected != executable {
		return Result{}, errors.Join(ErrConfiguration, err)
	}
	command := exec.Command(executable, slices.Clone(args)...)
	// OS controls use explicit arguments and a stable absolute directory.
	command.Dir = "/"
	command.Env = slices.Clone(r.environment)
	stdout := &boundedOutput{}
	command.Stdout = stdout
	// Raw manager diagnostics may contain inherited values; never retain them.
	command.Stderr = nil
	err = execution.RunIsolatedCommand(ctx, r.ownerExecutable, command)
	result := Result{Stdout: slices.Clone(stdout.bytes), ExitCode: -1}
	code, joined := execution.NativeCommandExitCode(err)
	if joined {
		result.ExitCode = code
		err = nil
	}
	if stdout.overflow {
		err = errors.Join(err, errors.New("gateway service manager output exceeds limit"))
	}
	return result, err
}

type boundedOutput struct {
	mu       sync.Mutex
	bytes    []byte
	overflow bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := maxCommandOutput - len(b.bytes)
	b.bytes = append(b.bytes, data[:min(len(data), remaining)]...)
	b.overflow = b.overflow || len(data) > remaining
	return len(data), nil
}
