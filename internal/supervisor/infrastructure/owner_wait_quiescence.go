package infrastructure

import (
	"context"
	"os"
)

// Minted only by an actual outstanding current-resident Wait observation.
// This grants pending projection, never native closure or launch authority.
type rootDaemonWaitQuiescencePendingError struct{}

func (*rootDaemonWaitQuiescencePendingError) Error() string { return ErrOwnerPending.Error() }

func (*rootDaemonWaitQuiescencePendingError) Unwrap() error                    { return ErrOwnerPending }
func (*rootDaemonWaitQuiescencePendingError) RootDaemonWaitQuiescencePending() {}

// RequireRootDaemonWaitQuiescence is a negative prerequisite for a cold
// predecessor Start, not native task closure. The caller retains bootstrap EX
// across this observation, acceptance and Start, serializing registrations.
func (w *daemonWaitOwner) RequireRootDaemonWaitQuiescence(ctx context.Context, root string) error {
	if w == nil || ctx == nil {
		return os.ErrInvalid
	}
	if root != w.origin.Root() {
		return ErrIdentity
	}
	if err := w.requireServing(ctx); err != nil {
		return err
	}
	if err := w.requireNoRegisteredWait(ctx); err != nil {
		return err
	}
	if err := w.requireNoHistoricalCustodian(ctx); err != nil {
		return err
	}
	if err := w.requireNoRegisteredWait(ctx); err != nil {
		return err
	}
	return w.requireServing(ctx)
}

func (w *daemonWaitOwner) requireNoRegisteredWait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.registrationClosed {
		return os.ErrClosed
	}
	for done := range w.active {
		select {
		case <-done:
		default:
			return &rootDaemonWaitQuiescencePendingError{}
		}
	}
	return nil
}
