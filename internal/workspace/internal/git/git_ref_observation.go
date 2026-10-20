package git

import (
	"context"
	"errors"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// gitSymbolicRefNotSymbolicExit is the clean status native symbolic-ref
// --quiet reports for a name that is not a symbolic ref: an absent, direct, or
// packed ref, or a detached HEAD. It never establishes absence by itself.
const gitSymbolicRefNotSymbolicExit = 1

var (
	errUnexpectedSymbolicRefOutput = errors.New("Git symbolic-ref reported a non-symbolic name with unexpected output")
	errMalformedSymbolicRefTarget  = errors.New("Git symbolic-ref reported a malformed symbolic target")
)

type gitExitCoder interface {
	ExitCode() int
}

// symbolicRefObservation is what one native symbolic-ref query proves about
// one name. A symbolic name may be live or dangling; its target need not be
// owned by Delegate.
type symbolicRefObservation struct {
	symbolic bool
	target   string
}

// observeSymbolicRef queries exactly one name without following a symbolic
// chain. for-each-ref omits dangling symbolic refs, so the inventory alone
// cannot distinguish one from an absent ref.
func (g Git) observeSymbolicRef(ctx context.Context, directory, name string) (symbolicRefObservation, error) {
	stdout, stderr, err := g.runOutput(ctx, directory, []string{"symbolic-ref", "--quiet", "--no-recurse", name}, nil, nil)
	return classifySymbolicRef(stdout, stderr, err)
}

// classifySymbolicRef decides a native symbolic-ref --quiet result. Exit zero
// names a symbolic target; one clean not-symbolic exit with empty streams
// means not symbolic; every other result is an operational error that keeps
// its cause.
func classifySymbolicRef(stdout, stderr []byte, runErr error) (symbolicRefObservation, error) {
	if runErr != nil {
		if !gitExitedOnlyWith(runErr, gitSymbolicRefNotSymbolicExit) {
			return symbolicRefObservation{}, gitCommandError(runErr, stderr)
		}
		if len(stdout) != 0 || len(stderr) != 0 {
			return symbolicRefObservation{}, gitCommandError(errors.Join(errUnexpectedSymbolicRefOutput, runErr), stderr)
		}
		return symbolicRefObservation{}, nil
	}
	target := removeOneTerminatingNewline(string(stdout))
	if target == "" || strings.ContainsAny(target, "\x00\n") || len(stderr) != 0 {
		return symbolicRefObservation{}, gitCommandError(errMalformedSymbolicRefTarget, stderr)
	}
	return symbolicRefObservation{symbolic: true, target: target}, nil
}

// gitExitedOnlyWith reports whether err records exactly one native Git exit
// with code and no independent fault. The execution runner joins stream,
// close, scope, and context errors with the wait result; any such cause, or a
// signal exit reported as -1, is not a clean exit.
func gitExitedOnlyWith(err error, code int) bool {
	exits := 0
	onlyExits := task.AllErrorCauses(err, func(cause error) bool {
		var exitCoder gitExitCoder
		if !errors.As(cause, &exitCoder) || exitCoder.ExitCode() != code {
			return false
		}
		exits++
		return true
	})
	return onlyExits && exits == 1
}
