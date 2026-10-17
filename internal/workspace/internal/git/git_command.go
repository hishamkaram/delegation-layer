package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

var gitRedirectionVariables = map[string]struct{}{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
	"GIT_COMMON_DIR":                   {},
	"GIT_DIR":                          {},
	"GIT_INDEX_FILE":                   {},
	"GIT_NAMESPACE":                    {},
	"GIT_OBJECT_DIRECTORY":             {},
	"GIT_OBJECT_DIRECTORY_RELATIVE":    {},
	"GIT_WORK_TREE":                    {},
}

var errGitRunnerRequired = errors.New("git command runner is required")

// ValidateGitEnvironment rejects inherited Git redirections before a caller
// authorizes any Git operation. It does not run a command or rewrite the
// process environment.
func ValidateGitEnvironment() error {
	return validateInheritedGitEnvironment(os.Environ())
}

func (g Git) executable() string {
	if g.Executable == "" {
		return defaultGitExecutable
	}
	return g.Executable
}

func (g Git) run(ctx context.Context, directory string, args []string, overrides map[string]string, stdin io.Reader, stdout, stderr io.Writer) error {
	if ctx == nil {
		return errors.Join(contract.ErrInvalidGitContext, errors.New("nil command context"))
	}
	if g.Runner == nil {
		return errGitRunnerRequired
	}
	if directory == "" {
		return errors.Join(contract.ErrInvalidGitContext, errors.New("empty git command directory"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	env, err := gitEnvironment(overrides)
	if err != nil {
		return err
	}
	commandArgs := make([]string, 1, len(args)+2)
	commandArgs[0] = "-C"
	commandArgs = append(commandArgs, directory)
	commandArgs = append(commandArgs, args...)
	command := exec.Command(g.executable(), commandArgs...)
	command.Dir = directory
	command.Env = env
	command.Stdin = stdin
	var boundedStderr *boundedBuffer
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		boundedStderr = &boundedBuffer{limit: 64 * 1024}
		stderr = boundedStderr
	}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := g.Runner.Run(ctx, command); err != nil {
		if boundedStderr != nil {
			err = gitCommandErrorWithBoundedStderr(err, boundedStderr)
		}
		return fmt.Errorf("git %s: %w", strings.Join(commandArgs, " "), err)
	}
	return nil
}

func gitEnvironment(overrides map[string]string) ([]string, error) {
	if err := ValidateGitEnvironment(); err != nil {
		return nil, err
	}

	overrideKeys := make([]string, 0, len(overrides))
	for key, value := range overrides {
		if key == "" || strings.ContainsRune(key, '=') || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("%w: invalid environment override", contract.ErrInvalidGitContext)
		}
		if _, blocked := gitRedirectionVariables[key]; blocked && key != "GIT_INDEX_FILE" {
			return nil, fmt.Errorf("%w: unsupported Git environment override %s", contract.ErrInvalidGitContext, key)
		}
		overrideKeys = append(overrideKeys, key)
	}
	sort.Strings(overrideKeys)

	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, blocked := gitRedirectionVariables[key]; blocked {
			continue
		}
		if _, replaced := overrides[key]; replaced {
			continue
		}
		environment = append(environment, entry)
	}
	for _, key := range overrideKeys {
		environment = append(environment, key+"="+overrides[key])
	}
	return environment, nil
}

func validateInheritedGitEnvironment(environment []string) error {
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, blocked := gitRedirectionVariables[key]; blocked && value != "" {
			return fmt.Errorf("%w: inherited %s is set", contract.ErrInvalidGitContext, key)
		}
	}
	return nil
}

func (g Git) runOutput(ctx context.Context, directory string, args []string, overrides map[string]string, stdin io.Reader) ([]byte, []byte, error) {
	stdout := &boundedBuffer{limit: 64 * 1024}
	stderr := &boundedBuffer{limit: 64 * 1024}
	err := g.run(ctx, directory, args, overrides, stdin, stdout, stderr)
	if stdout.exceeded || stderr.exceeded {
		err = errors.Join(err, errors.New("git command output exceeded the scalar bound"))
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func (g Git) runScalar(ctx context.Context, directory string, args []string, overrides map[string]string) (string, error) {
	stdout, stderr, err := g.runOutput(ctx, directory, args, overrides, nil)
	if err != nil {
		return "", gitCommandError(err, stderr)
	}
	return removeOneTerminatingNewline(string(stdout)), nil
}

// readRecordOutput keeps bulk Git inventories out of the scalar buffer. The
// injected runner still owns execution; this helper owns only its scratch file.
func (g Git) readRecordOutput(ctx context.Context, directory string, args []string, read func(io.Reader) error) (diagnostic []byte, resultErr error) {
	file, err := os.CreateTemp("", "delegate-git-records-")
	if err != nil {
		return nil, fmt.Errorf("create Git record scratch: %w", err)
	}
	path := file.Name()
	defer func() { resultErr = errors.Join(resultErr, file.Close(), os.Remove(path)) }()
	stderr := &boundedBuffer{limit: 64 * 1024}
	if err = g.run(ctx, directory, args, nil, nil, file, stderr); err != nil {
		return stderr.Bytes(), gitCommandError(err, stderr.Bytes())
	}
	if stderr.exceeded {
		return stderr.Bytes(), errors.New("Git record command stderr exceeded the scalar bound")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return stderr.Bytes(), fmt.Errorf("rewind Git record scratch: %w", err)
	}
	return stderr.Bytes(), read(file)
}

const gitStderrTruncationMarker = "[git stderr truncated at 65536 bytes]"

func gitCommandErrorWithBoundedStderr(err error, stderr *boundedBuffer) error {
	diagnostic := stderr.Bytes()
	if stderr.exceeded {
		diagnostic = make([]byte, 0, len(diagnostic)+len(gitStderrTruncationMarker)+1)
		diagnostic = append(diagnostic, stderr.Bytes()...)
		diagnostic = append(diagnostic, '\n')
		diagnostic = append(diagnostic, gitStderrTruncationMarker...)
	}
	return gitCommandError(err, diagnostic)
}

func gitCommandError(err error, stderr []byte) error {
	if err == nil {
		return nil
	}
	if len(stderr) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, removeOneTerminatingNewline(string(stderr)))
}

func removeOneTerminatingNewline(value string) string {
	if strings.HasSuffix(value, "\n") {
		return value[:len(value)-1]
	}
	return value
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if b.limit < 0 {
		return len(data), nil
	}
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if remaining > len(data) {
			remaining = len(data)
		}
		_, writeErr := b.Buffer.Write(data[:remaining])
		if writeErr != nil {
			return 0, writeErr
		}
	}
	if len(data) > remaining {
		b.exceeded = true
	}
	return len(data), nil
}

type presenceWriter struct {
	seen bool
}

func (w *presenceWriter) Write(data []byte) (int, error) {
	if len(data) != 0 {
		w.seen = true
	}
	return len(data), nil
}
