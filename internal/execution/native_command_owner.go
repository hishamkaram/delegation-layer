package execution

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// RunCommandOwnerMode is an internal entry point for the same compiled binary.
// Native process-wide ownership is bound only inside this dedicated process.
func RunCommandOwnerMode(args []string) (int, bool) {
	if code, handled := lifetime.RunProcessGroupPinMode(args); handled {
		return code, true
	}
	if len(args) == 0 || args[0] != nativeCommandMode {
		return 0, false
	}
	if len(args) != 2 || task.ValidateTaskID(args[1]) != nil {
		return 2, true
	}
	if err := runNativeOwner(args[1]); err != nil {
		return 1, true
	}
	return 0, true
}

func runNativeOwner(id string) (resultErr error) {
	connection, err := nativeConnection(os.NewFile(3, "native-command-owner"))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	if err = connection.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	request, err := readNativeFrame[nativeCommandRequest](connection)
	if err != nil {
		return err
	}
	if request.ID != id {
		return task.ErrIdentityMismatch
	}
	identity, err := decodeNativeCommand(request)
	if err != nil {
		return err
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	result := runNativeOwnerRequest(connection, identity)
	if err := connection.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return writeNativeFrame(connection, result)
}

func runNativeOwnerRequest(connection *net.UnixConn, identity nativeCommandIdentity) nativeCommandResult {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner, err := newNativeCommandLifetime(ctx, identity.id)
	if err != nil {
		return nativeCompletion(nil, err)
	}
	command := &exec.Cmd{Path: identity.executable, Args: identity.arguments, Dir: identity.directory, Env: os.Environ(), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	var complete atomic.Bool
	disconnected := make(chan error, 1)
	go func() {
		var signal [1]byte
		_, readErr := connection.Read(signal[:])
		if complete.Load() {
			disconnected <- nil
			return
		}
		cancel()
		// Any disconnect or cancellation request ends this probe. It does not
		// grant authority over processes outside the dedicated helper's tree.
		stopErr := owner.Terminate(context.WithoutCancel(ctx))
		if readErr != nil && !errors.Is(readErr, net.ErrClosed) {
			stopErr = errors.Join(stopErr, context.Canceled)
		}
		disconnected <- stopErr
	}()
	workErr, stopErr := RunSupervisedContext(ctx, identity.deadline, SupervisedOptions{Stopper: owner, Lifetime: owner}, func(scope PreflightScope) error {
		return RunScopedCommand(scope, command, CommandHooks{NullOutput: identity.nullOutput, Wait: nativeCallbackWait})
	})
	complete.Store(true)
	readCloseErr := connection.CloseRead()
	disconnectErr := <-disconnected
	closeErr := owner.close(context.WithoutCancel(ctx))
	result := nativeCompletion(command.ProcessState, errors.Join(workErr, stopErr, readCloseErr, disconnectErr, closeErr, ctx.Err()))
	if errors.Join(stopErr, readCloseErr, disconnectErr, closeErr, ctx.Err()) == nil && expectedNativeTimeout(workErr) {
		result.Fault = ""
		result.Expired = true
		result.Canceled = false
	}
	return result
}

func nativeCompletion(state *os.ProcessState, resultErr error) nativeCommandResult {
	result := nativeCommandResult{ExitCode: -1}
	if state != nil && state.Exited() {
		result.Exited, result.ExitCode = true, state.ExitCode()
	}
	if resultErr == nil {
		return result
	}
	exits := 0
	pureExit := task.AllErrorCauses(resultErr, func(leaf error) bool {
		var exit *exec.ExitError
		if !errors.As(leaf, &exit) || !exit.Exited() || exit.ExitCode() != result.ExitCode {
			return false
		}
		exits++
		return true
	})
	if pureExit && exits == 1 && result.Exited {
		return result
	}
	message := []rune(resultErr.Error())
	result.Fault = string(message[:min(len(message), 2048)])
	result.Canceled, result.Expired = errors.Is(resultErr, context.Canceled), errors.Is(resultErr, context.DeadlineExceeded)
	return result
}

// expectedNativeTimeout distinguishes the worker's finite deadline from helper
// or cleanup faults. It never interprets arbitrary error text.
func expectedNativeTimeout(err error) bool {
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errBudgetExpiredBeforeStart) {
		return false
	}
	return task.AllErrorCauses(err, func(leaf error) bool {
		if errors.Is(leaf, context.DeadlineExceeded) || errors.Is(leaf, context.Canceled) || errors.Is(leaf, errBudgetExpiredBeforeStart) {
			return true
		}
		var exit nativeCallbackWaitError
		return errors.As(leaf, &exit)
	})
}

// Keep callback Wait errors distinct from command-owner and cleanup errors.
// As preserves the ordinary native exit contract without erasing this origin.
type nativeCallbackWaitError struct{ exit *exec.ExitError }

func (e nativeCallbackWaitError) Error() string      { return "native callback exited unsuccessfully" }
func (e nativeCallbackWaitError) As(target any) bool { return errors.As(e.exit, target) }
func nativeCallbackWait(command *exec.Cmd) error {
	err := command.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nativeCallbackWaitError{exit: exit}
	}
	return err
}
