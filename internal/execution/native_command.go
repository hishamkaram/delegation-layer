package execution

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

// RunIsolatedCommand joins a dedicated one-shot owner. That process alone
// binds native containment; the caller's other descendants are never owned.
// The command's streams remain owned by RunScopedCommand, including failures.
func RunIsolatedCommand(ctx context.Context, ownerExecutable string, command *exec.Cmd) (resultErr error) {
	invocation, err := prepareNativeInvocation(ctx, ownerExecutable, command)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, invocation.close()) }()
	canceled := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { canceled <- invocation.connection.CloseWrite() })
	defer func() {
		if !stop() {
			resultErr = errors.Join(resultErr, <-canceled)
		}
	}()
	runErr := RunScopedCommand(nativeParentScope{ctx}, invocation.owner, CommandHooks{Started: invocation.started})
	if !invocation.launched {
		return runErr
	}
	deadline, parseErr := time.Parse(time.RFC3339Nano, invocation.request.Deadline)
	if parseErr != nil {
		return errors.Join(runErr, parseErr)
	}
	recorded, readErr := readNativeCompletion(invocation.connection, deadline)
	return errors.Join(runErr, readErr, nativeRecordedError(recorded), ctx.Err())
}

func readNativeCompletion(connection *net.UnixConn, deadline time.Time) (nativeCommandResult, error) {
	// A peer failure must remain finite even if an unrelated faulty process
	// retains a descriptor. Three native cleanup phases each own at most 10s.
	if err := connection.SetReadDeadline(deadline.Add(30 * time.Second)); err != nil {
		return nativeCommandResult{}, err
	}
	return readNativeFrame[nativeCommandResult](connection)
}

// One invocation owns the IPC endpoints and its helper command. Started is
// called synchronously by the pipe/Wait owner; close runs after that owner joins.
type nativeInvocation struct {
	connection *net.UnixConn
	child      *os.File
	owner      *exec.Cmd
	request    nativeCommandRequest
	launched   bool
}

func prepareNativeInvocation(ctx context.Context, ownerExecutable string, command *exec.Cmd) (*nativeInvocation, error) {
	if ctx == nil || command == nil {
		return nil, errors.New("native command requires context and command")
	}
	deadline, finite := ctx.Deadline()
	if !finite || ctx.Err() != nil {
		return nil, errors.Join(errors.New("native command requires an active finite deadline"), ctx.Err())
	}
	if err := validateNativeProcess(ownerExecutable, command); err != nil {
		return nil, err
	}
	id, err := randomid.NewTaskID()
	if err != nil {
		return nil, err
	}
	arguments := make([]string, len(command.Args))
	for index, argument := range command.Args {
		arguments[index] = base64.StdEncoding.EncodeToString([]byte(argument))
	}
	request := nativeCommandRequest{ID: id, ExecutableBase64: base64.StdEncoding.EncodeToString([]byte(command.Path)), ArgumentsBase64: arguments, DirectoryBase64: base64.StdEncoding.EncodeToString([]byte(command.Dir)), Deadline: deadline.UTC().Format(time.RFC3339Nano)}
	if err = validateNativeCommand(request); err != nil {
		return nil, err
	}
	connection, child, err := nativeSocketPair()
	if err != nil {
		return nil, err
	}
	owner := exec.Command(ownerExecutable, nativeCommandMode, id)
	owner.Dir, owner.Env, owner.Stdin = command.Dir, slices.Clone(command.Env), command.Stdin
	owner.Stdout, owner.Stderr, owner.ExtraFiles = command.Stdout, command.Stderr, []*os.File{child}
	invocation := &nativeInvocation{connection: connection, child: child, owner: owner, request: request}
	if err := lifetime.ConfigureIndependentSession(owner); err != nil {
		return nil, errors.Join(err, invocation.close())
	}
	return invocation, nil
}

func validateNativeProcess(ownerExecutable string, command *exec.Cmd) error {
	if !filepath.IsAbs(ownerExecutable) {
		return errors.New("native command owner must be an absolute executable")
	}
	// Only identity and streams cross the protocol. Process lifetime settings
	// are never copied into the fresh helper or native command.
	if command.Cancel != nil || command.SysProcAttr != nil || len(command.ExtraFiles) != 0 || command.Process != nil || command.ProcessState != nil {
		return errors.New("unsupported native command process state")
	}
	return nil
}

func (invocation *nativeInvocation) close() error {
	resultErr := invocation.connection.Close()
	if invocation.child != nil {
		resultErr = errors.Join(resultErr, invocation.child.Close())
	}
	return resultErr
}

func (invocation *nativeInvocation) started() error {
	invocation.launched = true
	closeErr := invocation.child.Close()
	invocation.child = nil
	if closeErr != nil {
		return errors.Join(closeErr, invocation.connection.CloseWrite())
	}
	deadline, err := time.Parse(time.RFC3339Nano, invocation.request.Deadline)
	if err != nil {
		return errors.Join(err, invocation.connection.CloseWrite())
	}
	if err := invocation.connection.SetWriteDeadline(deadline); err != nil {
		return errors.Join(err, invocation.connection.CloseWrite())
	}
	if err := writeNativeFrame(invocation.connection, invocation.request); err != nil {
		return errors.Join(err, invocation.connection.CloseWrite())
	}
	return nil
}

// The parent owns only its helper's Start/Wait. The helper receives the same
// absolute deadline and a disconnect channel; it owns native termination.
type nativeParentScope struct{ ctx context.Context }

func (scope nativeParentScope) Context() context.Context { return scope.ctx }
func (scope nativeParentScope) StartCommand(_ *exec.Cmd, start func() error) error {
	if err := scope.ctx.Err(); err != nil {
		return err
	}
	return start()
}
func (nativeParentScope) FinishCommand(*exec.Cmd) error { return nil }

func nativeRecordedError(result nativeCommandResult) error {
	if result.ExitCode < -1 || (result.Exited && result.ExitCode < 0) || (!result.Exited && result.Fault == "" && !result.Canceled && !result.Expired) {
		return errors.New("invalid native command completion")
	}
	var resultErr error
	if result.Exited && result.ExitCode != 0 {
		resultErr = nativeCommandExitError{code: result.ExitCode}
	}
	if result.Fault != "" {
		resultErr = errors.Join(resultErr, errors.New(result.Fault))
	}
	if result.Canceled {
		resultErr = errors.Join(resultErr, context.Canceled)
	}
	if result.Expired {
		resultErr = errors.Join(resultErr, context.DeadlineExceeded)
	}
	return resultErr
}
