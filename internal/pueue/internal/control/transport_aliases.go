package control

import (
	"context"
	"os/exec"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
)

type (
	CommandResult = transport.CommandResult
	Pending       = transport.Pending
	InFlightError = transport.InFlightError
)

var ErrInFlight = transport.ErrInFlight

func newPending(args []string) *Pending { return transport.NewPending(args) }

func newCompletedPending(args []string, result CommandResult) *Pending {
	return transport.NewCompletedPending(args, result)
}

func startOwned(command *exec.Cmd) *Pending { return transport.StartOwned(command) }

func startOwnedCaptured(command *exec.Cmd, commandID string, observer func(CommandEvent), statusVersion string) *Pending {
	return transport.StartOwnedCaptured(command, commandID, observer, statusVersion)
}

func runOwnedCommand(command *exec.Cmd, statusVersion string, onStarted func(CommandResult)) CommandResult {
	return transport.RunOwnedCommand(command, statusVersion, onStarted)
}

func observeCommand(ctx context.Context, pending *Pending, timeout time.Duration) (CommandResult, error) {
	return transport.ObserveCommand(ctx, pending, timeout)
}

func pendingFrom(err error) *Pending { return transport.PendingFrom(err) }

func pendingHandles(err error) []*Pending { return transport.PendingHandles(err) }

func awaitPending(ctx context.Context, pending *Pending) (CommandResult, error) {
	return transport.AwaitPending(ctx, pending)
}
