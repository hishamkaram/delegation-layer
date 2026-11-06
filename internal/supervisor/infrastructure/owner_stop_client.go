package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/localpeer"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// OwnerStopState describes only the selected infrastructure serving role. It
// says nothing about Pueue, tasks, other generations or global root quiescence.
type OwnerStopState string

const (
	OwnerServingReleased OwnerStopState = "serving_released"
	OwnerNotInitialized  OwnerStopState = "not_initialized"
	OwnerStopBusy        OwnerStopState = "busy"
	OwnerStopUnknown     OwnerStopState = "unknown"
)

// OwnerProcessClosure is independent of serving release. A live retired owner
// may remain the sole daemon waiter after its listener and lease are released.
type OwnerProcessClosure string

const (
	OwnerProcessClosed  OwnerProcessClosure = "closed"
	OwnerProcessPending OwnerProcessClosure = "pending"
	OwnerProcessUnknown OwnerProcessClosure = "unknown"
)

type OwnerStopResult struct {
	State          OwnerStopState      `json:"state"`
	ProcessClosure OwnerProcessClosure `json:"process_closure"`
	Startup        StartupReference    `json:"startup,omitzero"`
	// Process identifies this startup's selected incarnation, not its liveness.
	Process ProcessIdentity `json:"process,omitzero"`
}

// StopOwnerServing contacts one existing selected owner at most once. It never
// initializes, elects, repairs, starts or replays work. The caller owns its
// finite context and borrowed reader. A released serving role is not process
// closure: the same incarnation can remain the only waiter for a live daemon.
func StopOwnerServing(ctx context.Context, reader *taskdir.ReadOnlyStore) (OwnerStopResult, error) {
	result := OwnerStopResult{State: OwnerStopUnknown, ProcessClosure: OwnerProcessUnknown}
	if err := validateOwnerStopContext(ctx, reader); err != nil {
		return result, err
	}
	selected, found, err := readOwnerStopLocator(ctx, reader)
	if err != nil {
		return result, err
	}
	if !found {
		if err = requireNoOwnerStopResidue(ctx, reader); err != nil {
			return result, err
		}
		result.State = OwnerNotInitialized
		return result, nil
	}
	result.Startup = selected.reference
	if err = validateOwnerStopBoot(ctx, selected); err != nil {
		return result, err
	}
	result, err = observeOwnerStopClosure(ctx, reader, selected)
	if err != nil || result.State == OwnerServingReleased {
		return result, err
	}
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		return result, err
	}
	deadline, _ := ctx.Deadline() // The public boundary requires a finite caller.
	request := ownerStopRequest{StopProtocol: ownerStopProtocolVersion, RootID: selected.pin.RootID(), Owner: selected.reference, PublicationSHA256: selected.publicationSHA256, Client: ProcessIdentity{PID: self.PID(), StartToken: self.StartToken()}, Deadline: deadline.UTC().Format(time.RFC3339Nano)}
	reply, contactErr := contactOwnerStop(ctx, reader, selected, request)
	if reply != (ownerStopReply{}) {
		switch reply.Code {
		case ownerStopBusy:
			result.State = OwnerStopBusy
			return result, contactErr
		case ownerStopFault:
			return result, errors.Join(ErrOwnerRequest, contactErr)
		case ownerStopAccepted:
			observed, observeErr := awaitOwnerStopClosure(ctx, reader, selected, result)
			return observed, errors.Join(contactErr, observeErr)
		}
	}
	observed, observeErr := observeOwnerStopClosure(ctx, reader, selected)
	return observed, errors.Join(contactErr, observeErr)
}

func validateOwnerStopContext(ctx context.Context, reader *taskdir.ReadOnlyStore) error {
	if ctx == nil || reader == nil {
		return os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, finite := ctx.Deadline(); !finite {
		return os.ErrInvalid
	}
	return nil
}

func validateOwnerStopBoot(ctx context.Context, selected ownerStopLocator) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bootID, err := lifetime.BootID()
	if err != nil {
		return err
	}
	if lifetime.CompareBootIDs(selected.bootID, bootID) != lifetime.BootSame {
		return ErrIdentity
	}
	return ctx.Err()
}

func reobserveOwnerStopLocator(ctx context.Context, reader *taskdir.ReadOnlyStore, selected ownerStopLocator) (ownerStopLocator, error) {
	current, found, err := readOwnerStopLocator(ctx, reader)
	if err != nil {
		return current, err
	}
	if !found {
		return current, ErrIdentity
	}
	return matchSelectedOwnerStopLocator(ctx, current, selected)
}

func reobserveSelectedOwnerStopLocator(ctx context.Context, reader *taskdir.ReadOnlyStore, selected ownerStopLocator) (ownerStopLocator, error) {
	current, err := readOwnerStopSequence(ctx, reader, selected.pin, selected.reference.Sequence)
	if err != nil {
		return current, err
	}
	return matchSelectedOwnerStopLocator(ctx, current, selected)
}

func matchSelectedOwnerStopLocator(ctx context.Context, current, selected ownerStopLocator) (ownerStopLocator, error) {
	if current.pin != selected.pin || current.reference != selected.reference || current.assumption != selected.assumption || current.publication != selected.publication || current.publicationSHA256 != selected.publicationSHA256 || current.bootID != selected.bootID || (selected.servingReleased && !current.servingReleased) {
		return current, ErrIdentity
	}
	return current, validateOwnerStopBoot(ctx, current)
}

func ownerStopProcessPresent(ctx context.Context, selected ownerStopLocator) (bool, error) {
	if err := validateOwnerStopBoot(ctx, selected); err != nil {
		return false, err
	}
	reference, err := selected.assumption.Process.Reference()
	if err != nil {
		return false, err
	}
	present, err := lifetime.ProcessPresent(reference)
	return present, errors.Join(err, ctx.Err())
}

func observeOwnerStopClosure(ctx context.Context, reader *taskdir.ReadOnlyStore, selected ownerStopLocator) (OwnerStopResult, error) {
	result := OwnerStopResult{State: OwnerStopUnknown, ProcessClosure: OwnerProcessUnknown, Startup: selected.reference}
	current, err := reobserveSelectedOwnerStopLocator(ctx, reader, selected)
	if err != nil {
		return result, err
	}
	present, err := ownerStopProcessPresent(ctx, current)
	if err != nil {
		return result, err
	}
	result.Process = current.assumption.Process
	if present {
		result.ProcessClosure = OwnerProcessPending
	}
	if current.servingReleased || !present {
		successor, successorErr := ownerStopSuccessorAccepted(ctx, reader, current)
		if successorErr != nil {
			return result, successorErr
		}
		if successor {
			return observeReleasedOwnerStop(ctx, reader, current, result)
		}
	}
	err = probeExistingLease(ctx, filepath.Join(selected.pin.Root(), instanceLeaseName), func() error {
		var observeErr error
		result, observeErr = observeOwnerStopUnderLease(ctx, reader, current)
		return observeErr
	})
	return result, err
}

// The selected resident's immutable fence or exact process absence proves its
// serving release after another accepted birth holds the shared instance lease.
// Native endpoint absence remains an independent current observation.
func observeReleasedOwnerStop(ctx context.Context, reader *taskdir.ReadOnlyStore, selected ownerStopLocator, result OwnerStopResult) (OwnerStopResult, error) {
	_, err := observeEndpointSocket(selected.publication.Endpoint.Path)
	if !errors.Is(err, os.ErrNotExist) {
		return result, errors.Join(ErrIdentity, err)
	}
	if _, err = reobserveSelectedOwnerStopLocator(ctx, reader, selected); err != nil {
		return result, err
	}
	result.State = OwnerServingReleased
	if result.ProcessClosure != OwnerProcessPending {
		result.ProcessClosure = OwnerProcessClosed
	}
	return result, nil
}

func observeOwnerStopUnderLease(ctx context.Context, reader *taskdir.ReadOnlyStore, selected ownerStopLocator) (OwnerStopResult, error) {
	result := OwnerStopResult{State: OwnerStopUnknown, ProcessClosure: OwnerProcessUnknown, Startup: selected.reference}
	current, err := reobserveSelectedOwnerStopLocator(ctx, reader, selected)
	if err != nil {
		return result, err
	}
	present, err := ownerStopProcessPresent(ctx, current)
	if err != nil {
		return result, err
	}
	result.Process = current.assumption.Process
	socket, socketErr := observeEndpointSocket(current.publication.Endpoint.Path)
	if socketErr != nil && !errors.Is(socketErr, os.ErrNotExist) {
		return result, socketErr
	}
	if socketErr == nil {
		if err = matchContactSocket(socket, current.publication.Endpoint); err != nil {
			return result, err
		}
	}
	if _, err = reobserveSelectedOwnerStopLocator(ctx, reader, current); err != nil {
		return result, err
	}
	if !present {
		result.State, result.ProcessClosure = OwnerServingReleased, OwnerProcessClosed
	} else {
		result.ProcessClosure = OwnerProcessPending
		if current.servingReleased && errors.Is(socketErr, os.ErrNotExist) {
			result.State = OwnerServingReleased
		}
	}
	return result, nil
}

func awaitOwnerStopClosure(ctx context.Context, reader *taskdir.ReadOnlyStore, selected ownerStopLocator, latest OwnerStopResult) (OwnerStopResult, error) {
	for {
		observed, err := observeOwnerStopClosure(ctx, reader, selected)
		if err != nil {
			return latest, err
		}
		latest = observed
		if latest.State == OwnerServingReleased {
			return latest, nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return latest, ctx.Err()
		case <-timer.C:
		}
	}
}

func contactOwnerStop(ctx context.Context, reader *taskdir.ReadOnlyStore, selected ownerStopLocator, request ownerStopRequest) (reply ownerStopReply, resultErr error) {
	verified := false
	defer func() {
		if resultErr != nil {
			resultErr = ownerContactError{cause: errors.Join(resultErr, ownerContactContextError(ctx))}
			if !verified {
				reply = ownerStopReply{}
			}
		}
	}()
	if err := revalidateOwnerStopEndpoint(ctx, reader, selected); err != nil {
		return reply, err
	}
	connection, err := dialOwnerContact(ctx, selected.publication.Endpoint.Path)
	if err != nil {
		return reply, err
	}
	finish := watchOwnerContactCancellation(ctx, connection)
	defer func() { resultErr = errors.Join(resultErr, finish(), connection.Close()) }()
	if err = authenticateOwnerStopPeer(ctx, connection, selected); err != nil {
		return reply, err
	}
	if err = revalidateOwnerStopEndpoint(ctx, reader, selected); err != nil {
		return reply, err
	}
	if err = writeOwnerFrame(connection, request); err != nil {
		return reply, err
	}
	reply, err = readOwnerFrame[ownerStopReply](bufio.NewReader(connection))
	if err != nil {
		return reply, err
	}
	if reply.RootID != request.RootID || reply.Owner != request.Owner || reply.PublicationSHA256 != request.PublicationSHA256 {
		return reply, ErrIdentity
	}
	if _, err = reobserveSelectedOwnerStopLocator(ctx, reader, selected); err != nil {
		return reply, err
	}
	if err = checkClosedOwnerContactEndpoint(ctx, reader, selected.pin, selected.publication.Endpoint); err != nil {
		return reply, err
	}
	verified = true
	return reply, nil
}

func revalidateOwnerStopEndpoint(ctx context.Context, reader *taskdir.ReadOnlyStore, selected ownerStopLocator) error {
	if _, err := reobserveOwnerStopLocator(ctx, reader, selected); err != nil {
		return err
	}
	return checkOwnerContactEndpoint(ctx, reader, selected.pin, selected.publication.Endpoint)
}

func authenticateOwnerStopPeer(ctx context.Context, connection *net.UnixConn, selected ownerStopLocator) (resultErr error) {
	defer redactOwnerAuthenticationError(&resultErr)
	if err := validateOwnerStopBoot(ctx, selected); err != nil {
		return err
	}
	peer, err := localpeer.Read(connection)
	if err != nil {
		return err
	}
	if peer.UID != os.Geteuid() || peer.UID != selected.assumption.UID || peer.PID != selected.assumption.Process.PID {
		return ErrIdentity
	}
	return requireOwnerProcessPresent(ctx, selected.assumption.Process)
}

// Every supported owner birth creates the persistent instance lease before
// accepting its intent. Only absence of both proves no owner was initialized;
// it grants no cleanup/election authority or process-closure evidence.
func requireNoOwnerStopResidue(ctx context.Context, reader *taskdir.ReadOnlyStore) (resultErr error) {
	pin, err := reader.PinRootContext(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, requireContactPin(ctx, reader, pin)) }()
	residue := fmt.Errorf("%w: infrastructure owner lease or startup residue requires inspection", ErrIdentity)
	// A legitimate birth must create the instance inode before accepting its
	// intent. Recheck the journal and then require positive inode absence.
	if _, found, readErr := readOwnerStopLocator(ctx, reader); readErr != nil {
		return readErr
	} else if found {
		return residue
	}
	if _, err = os.Lstat(filepath.Join(pin.Root(), instanceLeaseName)); err == nil {
		return residue
	} else if !task.RecordAbsent(err) {
		return err
	}
	return ctx.Err()
}
