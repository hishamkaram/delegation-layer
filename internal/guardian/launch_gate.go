package guardian

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

const (
	LaunchGateMode       = "--lifetime-launch-gate"
	lifetimeLaunchGateFD = "--gate-fd"
	lifetimeProjectFD    = "--project-fd"
	lifetimeProjectID    = "--project-identity"
	lifetimeProjectPath  = "--project-path-context"
	lifetimeLaunchToken  = byte(1)
)

type LaunchGate interface {
	Started(error) error
	Release() error
	Abort() error
}

// ProjectDirectory borrows runner-owned custody only for a native Start. The
// factory never closes File. Legacy launches inherit custody; path launches
// carry only the saved binding and select their own directory after release.
type ProjectDirectory struct {
	File    *os.File
	Context task.ProjectLaunchContext
}

type LaunchGateFactory func(*exec.Cmd, *ProjectDirectory) (LaunchGate, error)

type runnerLifetimeLaunchGate struct {
	reader *os.File
	writer *os.File
}

// NewLaunchGateFactory wraps a provider in the current managed
// runner. The wrapper blocks inside the configured process scope until the
// guardian has durably recorded that exact PID, then execs the provider in the
// same process. Execution remains the sole owner of cmd.Wait.
func NewLaunchGateFactory(runner string) LaunchGateFactory {
	return func(cmd *exec.Cmd, project *ProjectDirectory) (LaunchGate, error) {
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
		projectArgs, err := project.arguments(3 + len(cmd.ExtraFiles))
		if err != nil {
			return nil, err
		}

		reader, writer, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("create lifetime launch gate: %w", err)
		}
		gate := &runnerLifetimeLaunchGate{reader: reader, writer: writer}
		if project != nil && !project.Context.Binding.Project.IsPath() {
			cmd.ExtraFiles = append(cmd.ExtraFiles, project.File)
		}
		gateFD := 3 + len(cmd.ExtraFiles)
		cmd.ExtraFiles = append(cmd.ExtraFiles, reader)
		cmd.Path = runner
		cmd.Args = []string{
			runner,
			LaunchGateMode,
			lifetimeLaunchGateFD,
			strconv.Itoa(gateFD),
		}
		cmd.Args = append(cmd.Args, projectArgs...)
		cmd.Args = append(cmd.Args, "--", providerPath)
		cmd.Args = append(cmd.Args, providerArgs...)
		return gate, nil
	}
}

func (p *ProjectDirectory) arguments(fd int) ([]string, error) {
	if p == nil {
		return nil, nil
	}
	observed := p.Context.Observed
	binding := p.Context.Binding
	if p.File == nil || !validProjectIdentity(observed) || !binding.Project.MatchesObservation(observed) {
		return nil, task.ErrIdentityMismatch
	}
	if err := validateProjectFile(p.File, observed); err != nil {
		return nil, err
	}
	if binding.Project.IsPath() {
		if err := validatePathProjectBinding(binding); err != nil {
			return nil, err
		}
		encoded, err := task.MarshalCanonical(binding)
		if err != nil || len(encoded) > task.MaxControlRecordSize {
			return nil, errors.Join(task.ErrEvidenceFault, err)
		}
		return []string{lifetimeProjectPath, string(encoded)}, nil
	}
	identity, err := task.MarshalCanonical(observed)
	if err != nil {
		return nil, err
	}
	return []string{lifetimeProjectFD, strconv.Itoa(fd), lifetimeProjectID, string(identity)}, nil
}

func validProjectIdentity(identity task.DirectoryIdentity) bool {
	return identity.Kind == "" && identity.Validate() == nil
}

func validatePathProjectBinding(binding task.ConnectorContext) error {
	if !binding.Project.IsPath() {
		return task.ErrIdentityMismatch
	}
	return task.ValidateConnectorBinding(&task.TaskRecord{CanonicalCwd: binding.Project.Path, ConnectorContext: &binding})
}

func validateProjectFile(file *os.File, identity task.DirectoryIdentity) error {
	var held unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &held); err != nil {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	if held.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(held.Dev) != identity.Device || uint64(held.Ino) != identity.Inode {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (g *runnerLifetimeLaunchGate) Started(startErr error) error {
	if g == nil {
		return task.ErrEvidenceFault
	}
	closeErr := closeLifetimeLaunchGateFile(&g.reader)
	if startErr != nil {
		closeErr = errors.Join(closeErr, closeLifetimeLaunchGateFile(&g.writer))
	}
	return closeErr
}

func (g *runnerLifetimeLaunchGate) Release() error {
	if g == nil || g.writer == nil {
		return task.ErrEvidenceFault
	}
	_, writeErr := g.writer.Write([]byte{lifetimeLaunchToken})
	closeErr := closeLifetimeLaunchGateFile(&g.writer)
	return errors.Join(writeErr, closeErr)
}

func (g *runnerLifetimeLaunchGate) Abort() error {
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

func RunLaunchGate(args []string, execProcess func(string, []string, []string) error) error {
	parsed, err := parseLifetimeLaunchGateArguments(args)
	if err != nil || execProcess == nil {
		return errors.Join(task.ErrEvidenceFault, err)
	}
	gate := os.NewFile(uintptr(parsed.gateFD), "delegate-provider-launch-gate")
	if gate == nil {
		return task.ErrEvidenceFault
	}
	var token [1]byte
	_, readErr := io.ReadFull(gate, token[:])
	closeErr := gate.Close()
	if readErr != nil || closeErr != nil || token[0] != lifetimeLaunchToken {
		return errors.Join(task.ErrEvidenceFault, readErr, closeErr)
	}
	if parsed.projectFD != 0 {
		if err = enterLaunchProject(parsed.projectFD, parsed.project); err != nil {
			return err
		}
	}
	if parsed.pathContext != nil {
		if err = enterPathLaunchProject(*parsed.pathContext); err != nil {
			return err
		}
	}
	return execProcess(parsed.providerPath, parsed.providerArgs, os.Environ())
}

type lifetimeLaunchArguments struct {
	gateFD       int
	projectFD    int
	project      task.DirectoryIdentity
	pathContext  *task.ConnectorContext
	providerPath string
	providerArgs []string
}

func parseLifetimeLaunchGateArguments(args []string) (parsed lifetimeLaunchArguments, err error) {
	if len(args) < 5 || args[0] != lifetimeLaunchGateFD {
		return parsed, task.ErrEvidenceFault
	}
	parsed.gateFD, err = strconv.Atoi(args[1])
	if err != nil || parsed.gateFD < 3 {
		return parsed, task.ErrEvidenceFault
	}
	remaining := args[2:]
	switch remaining[0] {
	case lifetimeProjectFD:
		remaining, err = parsed.readProjectArguments(remaining)
	case lifetimeProjectPath:
		remaining, err = parsed.readPathProjectArguments(remaining)
	}
	if err != nil {
		return parsed, err
	}
	if len(remaining) < 3 || remaining[0] != "--" {
		return parsed, task.ErrEvidenceFault
	}
	parsed.providerPath, parsed.providerArgs = remaining[1], slices.Clone(remaining[2:])
	if !filepath.IsAbs(parsed.providerPath) || filepath.Clean(parsed.providerPath) != parsed.providerPath || len(parsed.providerArgs) == 0 || parsed.providerArgs[0] == "" {
		return parsed, task.ErrEvidenceFault
	}
	return parsed, nil
}

func (p *lifetimeLaunchArguments) readPathProjectArguments(args []string) ([]string, error) {
	if len(args) < 5 || len(args[1]) > task.MaxControlRecordSize {
		return nil, task.ErrEvidenceFault
	}
	var binding task.ConnectorContext
	if err := task.DecodeStrict([]byte(args[1]), &binding); err != nil {
		return nil, errors.Join(task.ErrIdentityMismatch, err)
	}
	if err := validatePathProjectBinding(binding); err != nil {
		return nil, err
	}
	p.pathContext = &binding
	return args[2:], nil
}

func (p *lifetimeLaunchArguments) readProjectArguments(args []string) ([]string, error) {
	if len(args) < 7 || args[2] != lifetimeProjectID || len(args[3]) > task.MaxControlRecordSize {
		return nil, task.ErrEvidenceFault
	}
	var err error
	p.projectFD, err = strconv.Atoi(args[1])
	if err != nil || p.projectFD < 3 || p.projectFD == p.gateFD {
		return nil, task.ErrEvidenceFault
	}
	if err = task.DecodeStrict([]byte(args[3]), &p.project); err != nil || !validProjectIdentity(p.project) {
		return nil, errors.Join(task.ErrIdentityMismatch, err)
	}
	return args[4:], nil
}

// Only the single child bootstrap calls this process-wide chdir operation.
// Parent runners retain their own cwd and their original custody descriptor.
func enterLaunchProject(fd int, identity task.DirectoryIdentity) (resultErr error) {
	file := os.NewFile(uintptr(fd), "delegate-provider-project")
	if file == nil {
		return task.ErrIdentityMismatch
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if err := validateProjectFile(file, identity); err != nil {
		return err
	}
	var visible unix.Stat_t
	if err := unix.Stat(identity.Path, &visible); err != nil {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	if visible.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(visible.Dev) != identity.Device || uint64(visible.Ino) != identity.Inode {
		return task.ErrIdentityMismatch
	}
	if err := file.Chdir(); err != nil {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	if err := unix.Stat(".", &visible); err != nil {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	if uint64(visible.Dev) != identity.Device || uint64(visible.Ino) != identity.Inode {
		return task.ErrIdentityMismatch
	}
	return nil
}

// The child selects the current path only after the guardian's positive token.
// The selection owns its descriptors and closes all of them before provider exec.
func enterPathLaunchProject(binding task.ConnectorContext) (resultErr error) {
	selection, err := taskdir.OpenPathProjectDirectory(context.Background(), binding)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, selection.Close()) }()
	if err = selection.File().Chdir(); err != nil {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	return nil
}
