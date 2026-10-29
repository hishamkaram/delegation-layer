package execution

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// errInvalidBoundCommand reports a binder that returned no command and no error.
var errInvalidBoundCommand = errors.New("command binder returned no verified command")

// CommandBinder is the consumer-owned port that binds one verified provider or
// native inspection command to the bytes admitted for its executable. The owner
// that installs a binder decides where the command's view lives and who removes
// it. This package only calls the binder, then owns the returned command's
// Start, Wait and Close exactly as it does for a private copy.
//
// On error the binder returns no command, has closed every handle it opened for
// this attempt and has removed any partial per-attempt view. Durable state that
// is not per attempt, such as a pool owner's witness, shared verified images
// and crash leftovers, stays with its owner and is reclaimed under that owner's
// closure. A binder error is a definite setup refusal: nothing is started and
// no other binding is tried. The one exception is an initial binding with
// runtime refresh whose every cause is verifiedexec.ErrAdmittedExecutableChanged:
// it defers to the fresh preflight, which binds the refreshed plan once through
// the same binder. A binder propagates that error unchanged or through single
// %w wrappers; any joined cause of another kind refuses.
//
// ctx is the binding owner's context: a refreshed provider binding and every
// native inspection binding receive the scope's deadline context; the initial
// provider binding precedes the budget and receives context.Background, as the
// historical private copy had no context.
type CommandBinder interface {
	BindCommand(ctx context.Context, request verifiedexec.CommandRequest) (*verifiedexec.Command, error)
}

// bindCommand is the single binding decision for provider and inspection
// commands. Without a supplied binder it makes the historical private copy,
// which Command.Close removes.
func bindCommand(ctx context.Context, binder CommandBinder, request verifiedexec.CommandRequest) (*verifiedexec.Command, error) {
	if binder == nil {
		return verifiedexec.NewCommandFromRequest(request)
	}
	command, err := binder.BindCommand(ctx, request)
	if err != nil {
		return nil, err
	}
	if command == nil || command.Cmd == nil {
		var closeErr error
		if command != nil {
			closeErr = command.Close()
		}
		return nil, errors.Join(errInvalidBoundCommand, closeErr)
	}
	return command, nil
}
