package infrastructure

import (
	"context"
	"errors"
	"os"
)

// RunOwnerMode is the fixed native owner entry. Recognition precedes all FD3
// inspection, including malformed invocations. The entry exclusively owns its
// resident resources; the server and both completion owners borrow them until
// admission stops and all accepted operations have actually drained. A returned
// error or process exit never certifies canonical daemon/root closure.
func RunOwnerMode(ctx context.Context, args []string, host EntryHost) (bool, error) {
	invocation, handled, err := parseOwnerInvocation(args)
	if !handled || err != nil {
		return handled, err
	}
	if ctx == nil || host == nil {
		return true, os.ErrInvalid
	}
	entry, err := consumeOwnerInvocation(ctx, invocation, host)
	if err != nil {
		if entry != nil {
			err = closeResidentEntry(context.WithoutCancel(ctx), entry, err)
		}
		return true, err
	}
	server, err := newOwnerServer(ctx, entry, host)
	if err != nil {
		return true, closeResidentEntry(context.WithoutCancel(ctx), entry, err)
	}
	return true, runResidentServer(ctx, server)
}

// The startup timer belongs only to initial preparation. The serving lifetime
// remains live after that preparation succeeds or its original deadline passes.
// On failure, retain and join both actors before disposing of their borrowers.
func runResidentServer(ctx context.Context, server *ownerServer) error {
	serving, cancelServing := context.WithCancel(ctx)
	defer cancelServing()
	served := make(chan error, 1)
	prepared := make(chan error, 1)
	go func() { served <- server.serve(serving) }()
	go func() {
		_, err := server.prepareInitial(context.WithoutCancel(ctx))
		prepared <- err
	}()
	var serveErr, prepareErr error
	select {
	case prepareErr = <-prepared:
		if prepareErr != nil && !server.keepsRuntimeReconciliation() {
			server.awaitInitialHandoff(serving)
			cancelServing()
		}
		serveErr = <-served
	case serveErr = <-served:
		// Closing admission prevents a not-yet-registered initial preparation.
		// An admitted preparation retains its original finite deadline.
		server.stopAccepting()
		prepareErr = <-prepared
	}
	cancelServing()
	// No convenience timeout grants permission to close live borrowers. Every
	// accepted operation already owns its immutable finite deadline.
	drainErr := server.stopAcceptAndDrain(context.WithoutCancel(ctx))
	// stopAcceptAndDrain can report retained native cleanup faults after drain;
	// the actual drained signal, not its error value, controls entry disposal.
	<-server.drained
	return finishResidentRuntime(context.WithoutCancel(ctx), server.entry, server.waits, errors.Join(serveErr, prepareErr, drainErr))
}

// An unresolved close transfers the actual partial owner in the error graph.
// Reporting its cause alone would discard the resources that must outlive it.
func closeResidentEntry(ctx context.Context, entry *ownedResidentEntry, cause error) error {
	if err := entry.closeContext(ctx); err != nil {
		return errors.Join(cause, &residentEntryCleanupError{entry: entry, cause: err})
	}
	return cause
}

type residentEntryCleanupError struct {
	entry *ownedResidentEntry
	waits *daemonWaitOwner
	cause error
}

func (*residentEntryCleanupError) Error() string {
	return "infrastructure resident entry cleanup unresolved"
}
func (e *residentEntryCleanupError) Unwrap() error         { return e.cause }
func (*residentEntryCleanupError) RetainsResidentCleanup() {}
