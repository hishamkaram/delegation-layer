//go:build linux || darwin

package lifetime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"golang.org/x/sys/unix"
)

// This owner-local fixture needs private identity-verified signal primitives.
// Keeping it here avoids a production signal API or a second lifetime owner.
// Python owns its PTY and private paths; this guardian owns the installed TUI's
// Start, sole Wait, signal requests and independently proven scope closure.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--test-tui-terminal-raw-exit" {
		if err := leaveTerminalRaw(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "--test-tui-terminal" {
		if err := runTerminalUIFixture(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if code, handled := RunProcessGroupPinMode(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func leaveTerminalRaw() error {
	// This fixed negative control intentionally omits Restore on its own PTY.
	if _, err := term.MakeRaw(os.Stdin.Fd()); err != nil {
		return err
	}
	_, err := os.Stdout.Write([]byte("\x1b[?1049h\x1b[?1049l"))
	return err
}

type terminalUIReceipt struct {
	ChildPID        int    `json:"child_pid"`
	ChildExitCode   int    `json:"child_exit_code"`
	SignalRequested string `json:"signal_requested"`
	ExactWait       bool   `json:"exact_wait"`
	ScopeEmpty      bool   `json:"scope_empty"`
	FailureCleanup  bool   `json:"failure_cleanup"`
}

// Closing done publishes Wait's immutable result to success and cleanup
// observers. A trigger watcher never consumes the only completion receipt.
type terminalUIChild struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
}

func (c *terminalUIChild) wait(ctx context.Context, state *terminalUIReceipt) error {
	select {
	case <-c.done:
		state.ExactWait = true
		state.ChildExitCode = c.command.ProcessState.ExitCode()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type terminalUIFixture struct {
	scope *Scope
	child *terminalUIChild
	refs  []LaunchRef
	state terminalUIReceipt
}

func runTerminalUIFixture(args []string) (resultErr error) {
	if err := validateTerminalUIArgs(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id, err := randomid.NewTaskID()
	if err != nil {
		return err
	}
	plan, err := NewPlan(KindProcessGroup, AssuranceCooperative, "pg:"+id)
	if err != nil {
		return err
	}
	f := &terminalUIFixture{state: terminalUIReceipt{ChildExitCode: -1}}
	f.scope, err = CreatePreparedContext(ctx, plan)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, f.finish(args[4])) }()
	if err = f.scope.BindOwnerProcess(); err != nil {
		return err
	}
	if err = f.start(args[0], args[1], args[2]); err != nil {
		return err
	}
	if err = f.requestSignal(ctx, args[0], args[3]); err != nil {
		return err
	}
	if err = f.child.wait(ctx, &f.state); err != nil {
		return err
	}
	expected := 0
	switch args[0] {
	case "term":
		expected = 143
	case "hup", "hangup":
		expected = 129
	}
	if f.state.ChildExitCode != expected {
		return errors.Join(fmt.Errorf("TUI %s exit=%d, expected=%d", args[0], f.state.ChildExitCode, expected), f.child.err)
	}
	return nil
}

func (f *terminalUIFixture) requestSignal(ctx context.Context, mode, trigger string) error {
	var signal syscall.Signal
	switch mode {
	case "term":
		signal = syscall.SIGTERM
	case "hup":
		signal = syscall.SIGHUP
	default:
		return nil
	}
	if err := awaitTerminalUITrigger(ctx, trigger, f.child.done); err != nil {
		return err
	}
	ref, err := NewProcessRef(f.refs[0].PID(), f.refs[0].StartToken())
	if err != nil {
		return err
	}
	f.state.SignalRequested = mode
	return nativeSchedulerHostSignal(ctx, ref, signal)
}

func validateTerminalUIArgs(args []string) error {
	if len(args) != 5 {
		return errors.New("usage: --test-tui-terminal interactive|term|hup|hangup|raw-exit INSTALLED_DELEGATE ABS_ROOT ABS_TRIGGER ABS_RECEIPT")
	}
	switch args[0] {
	case "interactive", "term", "hup", "hangup", "raw-exit":
	default:
		return errors.New("unsupported fixed TUI fixture mode")
	}
	for _, path := range args[1:] {
		if !filepath.IsAbs(path) {
			return errors.New("TUI fixture requires absolute paths")
		}
	}
	in, err := os.Stdin.Stat()
	if err != nil {
		return err
	}
	out, err := os.Stdout.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(in, out) {
		return errors.New("TUI fixture requires one borrowed terminal for stdin and stdout")
	}
	_, err = unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
	return err
}

func (f *terminalUIFixture) start(mode, binary, root string) error {
	command := exec.Command(binary, "tui", "--root", root)
	if mode == "raw-exit" {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		command = exec.Command(self, "--test-tui-terminal-raw-exit")
	}
	command.Env = os.Environ()
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := f.scope.Configure(command); err != nil {
		return err
	}
	// This fixture creates a fresh cooperative scope, never a recovered/pinned
	// group. Kernel terminal ownership and delivered resize/hangup behavior are
	// the proof, rather than the private attribute shape. Setctty claims this
	// invocation's PTY; Ctty=0 names the child's stdin descriptor.
	if command.SysProcAttr == nil {
		return errors.New("TUI fixture has no configured native process attributes")
	}
	command.SysProcAttr.Setctty = true
	command.SysProcAttr.Ctty = 0
	if err := command.Start(); err != nil {
		return err
	}
	f.state.ChildPID = command.Process.Pid
	f.child = &terminalUIChild{command: command, done: make(chan struct{})}
	// Observe before Wait can reap a short-lived child. Both native backends
	// accept an unreaped zombie's identity; Wait remains owned on every result.
	ref, err := f.scope.ObserveStarted(command.Process.Pid)
	go func() {
		f.child.err = command.Wait()
		close(f.child.done)
	}()
	if err == nil {
		f.refs = []LaunchRef{ref}
	}
	return err
}

func (f *terminalUIFixture) finish(receiptPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var resultErr error
	if f.child != nil {
		select {
		case <-f.child.done:
		default:
			f.state.FailureCleanup = true
			resultErr = f.scope.Terminate(ctx, f.refs)
		}
		resultErr = errors.Join(resultErr, f.child.wait(ctx, &f.state))
	}
	empty, err := f.scope.EmptyContext(ctx, f.refs)
	f.state.ScopeEmpty = empty && err == nil
	resultErr = errors.Join(resultErr, err)
	if f.state.ScopeEmpty {
		resultErr = errors.Join(resultErr, f.scope.DestroyEmpty())
	}
	return errors.Join(resultErr, f.scope.Release(), writeTerminalUIReceipt(receiptPath, f.state))
}

func awaitTerminalUITrigger(ctx context.Context, path string, child <-chan struct{}) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Lstat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-child:
			return errors.New("TUI exited before native signal trigger")
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func writeTerminalUIReceipt(path string, value terminalUIReceipt) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if err = errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
