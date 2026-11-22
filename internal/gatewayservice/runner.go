package gatewayservice

import (
	"context"
	"errors"
	"io"
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
	stdout := &boundedOutput{}
	code, err := r.run(ctx, executable, args, stdout, false)
	result := Result{Stdout: slices.Clone(stdout.bytes), ExitCode: code}
	if stdout.overflow {
		err = errors.Join(err, errors.New("gateway service manager output exceeds limit"))
	}
	return result, err
}

func (r NativeRunner) Probe(ctx context.Context, executable string, args []string) (int, error) {
	return r.run(ctx, executable, args, io.Discard, true)
}

func (r NativeRunner) run(ctx context.Context, executable string, args []string, stdout io.Writer, nullOutput bool) (int, error) {
	if err := finite(ctx); err != nil {
		return 0, err
	}
	selected, err := verifiedexec.LocatePath(executable)
	if err != nil || selected != executable {
		return 0, errors.Join(ErrConfiguration, err)
	}
	command := exec.Command(executable, slices.Clone(args)...)
	// OS controls use explicit arguments and a stable absolute directory.
	command.Dir = "/"
	command.Env = slices.Clone(r.environment)
	command.Stdout = stdout
	// Raw manager diagnostics may contain inherited values; never retain them.
	command.Stderr = nil
	if nullOutput {
		err = execution.RunIsolatedCommandNullOutput(ctx, r.ownerExecutable, command)
	} else {
		err = execution.RunIsolatedCommand(ctx, r.ownerExecutable, command)
	}
	code, joined := execution.NativeCommandExitCode(err)
	if joined {
		if code < 0 || code > 255 {
			return -1, ErrManager
		}
		return code, nil
	}
	return -1, err
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
