package execution

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// errInvalidBoundCommand reports a binder that returned no command and no error.
var errInvalidBoundCommand = errors.New("command binder returned no verified command")

// CommandBinder is the consumer-owned port that binds one integrity-bound
// native helper command, started through PreflightScope, to the bytes admitted
// for that helper. The installed provider CLI never passes through it: provider
// launches and provider probes run the installed executable directly. The owner
// that installs a binder decides where the command's view lives and who removes
// it. This package only calls the binder, then owns the returned command's
// Start, Wait and Close.
//
// On error the binder returns no command, has closed every handle it opened for
// this attempt and has removed any partial per-attempt view. Durable state that
// is not per attempt stays with its owner and is reclaimed under that owner's
// closure. A binder error is a definite setup refusal: nothing is started and
// no other binding is tried. ctx is the scope's deadline context.
type CommandBinder interface {
	BindCommand(ctx context.Context, request verifiedexec.CommandRequest) (*verifiedexec.Command, error)
}

// bindCommand is the single binding decision for native helper commands.
// Without a supplied binder it makes the helper's private verified copy, which
// Command.Close removes.
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
