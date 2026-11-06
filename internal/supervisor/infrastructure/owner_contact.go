package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

// The caller owns the borrowed reader and actual pin throughout this one contact.
// Immutable records and fresh native observations bind the request; no contact
// snapshot selects a new head or certifies private-supervisor readiness.
// A failed write attempt without a verified reply has an unknown outcome and
// must never authorize a fresh Start. Verified replies survive cleanup errors;
// callers must retain that known outcome alongside any cleanup diagnostic.
func contactOwner(ctx context.Context, store *taskdir.ReadOnlyStore, pin taskdir.RootPin, request ownerRequest) (reply ownerReply, resultErr error) {
	verified := false
	var contextErr, cleanupErr error
	defer func() { reply, resultErr = completeOwnerContact(verified, reply, resultErr, contextErr, cleanupErr) }()
	operation, cancel, err := ownerContactContext(ctx, request)
	if err != nil {
		return ownerReply{}, err
	}
	defer cancel()
	intent, assumption, publication, err := readOwnerContactRecords(operation, store, pin, request)
	if err != nil {
		return ownerReply{}, err
	}
	// The current boot was checked before observing this native PID incarnation.
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		return ownerReply{}, err
	}
	if request.Client != (ProcessIdentity{PID: self.PID(), StartToken: self.StartToken()}) {
		return ownerReply{}, ErrIdentity
	}
	unixConnection, err := dialOwnerContact(operation, publication.Endpoint.Path)
	if err != nil {
		return ownerReply{}, err
	}
	finish := watchOwnerContactCancellation(operation, unixConnection)
	defer func() {
		cleanupErr = errors.Join(finish(), unixConnection.Close())
		if resultErr != nil {
			contextErr = ownerContactContextError(operation)
		}
	}()
	if err = authenticateOwnerPeer(operation, unixConnection, intent, assumption, publication); err != nil {
		return ownerReply{}, err
	}
	if err = checkOwnerContactEndpoint(operation, store, pin, publication.Endpoint); err != nil {
		return ownerReply{}, err
	}
	if err = writeOwnerFrame(unixConnection, request); err != nil {
		return ownerReply{}, err
	}
	reply, err = readOwnerFrame[ownerReply](bufio.NewReader(unixConnection))
	if err != nil {
		return ownerReply{}, err
	}
	if err = validateOwnerReplyBinding(intent, assumption, publication, request, reply); err != nil {
		return ownerReply{}, err
	}
	if err = readOwnerContactAfterReply(operation, store, pin, request, reply); err != nil {
		return ownerReply{}, err
	}
	verified = true
	return reply, nil
}

// An authenticated explicit initial failure may outlive its listener. Only the
// original published socket or its owned absence is accepted; no readiness is
// inferred, and replacements or changed canonical subjects remain unknown.
func readOwnerContactAfterReply(ctx context.Context, store *taskdir.ReadOnlyStore, pin taskdir.RootPin, request ownerRequest, reply ownerReply) error {
	intent, assumption, publication, err := readOwnerContactBinding(ctx, store, pin, request)
	if err != nil {
		return err
	}
	if err = validateOwnerReplyBinding(intent, assumption, publication, request, reply); err != nil {
		return err
	}
	if reply.initialFailureFor(request, intent) {
		return checkClosedOwnerContactEndpoint(ctx, store, pin, publication.Endpoint)
	}
	return checkOwnerContactEndpoint(ctx, store, pin, publication.Endpoint)
}

func (r ownerReply) initialFailureFor(request ownerRequest, intent StartupIntent) bool {
	if request.InitialResultFor == (StartupReference{}) || request.InitialResultFor != request.Owner || intent.Inputs.Kind != StartupRootOwner || request.preparationOperation().normalized() != intent.Inputs.Initial.normalized() {
		return false
	}
	if request.Context != (LaunchContextReference{}) && request.Context != intent.Inputs.Context {
		return false
	}
	switch r.Code {
	case ownerFault, ownerBusy, ownerIncompatible, ownerTimeout:
		return r.Failure != (PreparationFailure{})
	case ownerOK, ownerPending, ownerRefused:
		return false
	}
	return false
}

// A successful dial transfers exactly one connection to contactOwner.
func dialOwnerContact(ctx context.Context, path string) (*net.UnixConn, error) {
	deadline, finite := ctx.Deadline()
	if !finite {
		return nil, os.ErrInvalid
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return nil, errors.Join(ErrIdentity, connection.Close())
	}
	if err = unixConnection.SetDeadline(deadline); err != nil {
		return nil, errors.Join(err, unixConnection.Close())
	}
	return unixConnection, nil
}

func ownerContactContext(ctx context.Context, request ownerRequest) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := request.validate(); err != nil {
		return nil, nil, err
	}
	deadline, err := canonicalStartupTime(request.Deadline)
	if err != nil {
		return nil, nil, err
	}
	if !time.Now().Before(deadline) {
		return nil, nil, context.DeadlineExceeded
	}
	operation, cancel := context.WithDeadline(ctx, deadline)
	if err = operation.Err(); err != nil {
		cancel()
		return nil, nil, err
	}
	return operation, cancel, nil
}

// Native socket deadline expiry may race the context timer's delivery.
func ownerContactContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, finite := ctx.Deadline(); finite && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func readOwnerContactRecords(ctx context.Context, store *taskdir.ReadOnlyStore, pin taskdir.RootPin, request ownerRequest) (StartupIntent, StartupAssumption, OwnerPublication, error) {
	intent, assumption, publication, err := readOwnerContactBinding(ctx, store, pin, request)
	if err == nil {
		err = checkOwnerContactEndpoint(ctx, store, pin, publication.Endpoint)
	}
	if err != nil {
		return StartupIntent{}, StartupAssumption{}, OwnerPublication{}, err
	}
	return intent, assumption, publication, nil
}

func readOwnerContactBinding(ctx context.Context, store *taskdir.ReadOnlyStore, pin taskdir.RootPin, request ownerRequest) (StartupIntent, StartupAssumption, OwnerPublication, error) {
	var zeroIntent StartupIntent
	var zeroAssumption StartupAssumption
	var zeroPublication OwnerPublication
	intent, err := readOwnerContactIntent(ctx, store, pin, request.Owner)
	if err != nil {
		return zeroIntent, zeroAssumption, zeroPublication, err
	}
	assumption, err := readStartupAssumption(ctx, store, request.Owner)
	if err != nil {
		return zeroIntent, zeroAssumption, zeroPublication, err
	}
	publication, err := readOwnerPublication(ctx, store, request.Owner)
	if err != nil {
		return zeroIntent, zeroAssumption, zeroPublication, err
	}
	digest, err := publication.Digest(intent, assumption)
	if err != nil {
		return zeroIntent, zeroAssumption, zeroPublication, err
	}
	if err = validateOwnerRequestSubject(intent, digest, request); err != nil {
		return zeroIntent, zeroAssumption, zeroPublication, err
	}
	return intent, assumption, publication, nil
}

func readOwnerContactIntent(ctx context.Context, store *taskdir.ReadOnlyStore, pin taskdir.RootPin, reference StartupReference) (StartupIntent, error) {
	current, err := store.PinRootContext(ctx)
	if err != nil {
		return StartupIntent{}, err
	}
	if !current.Equal(pin) {
		return StartupIntent{}, ErrIdentity
	}
	intent, found, err := readLatestIntent(ctx, store)
	if err != nil {
		return StartupIntent{}, err
	}
	if !found {
		return StartupIntent{}, os.ErrNotExist
	}
	bootID, err := lifetime.BootID()
	if err != nil {
		return StartupIntent{}, err
	}
	if err = validateCurrentIntent(intent, pin, bootID); err != nil {
		return StartupIntent{}, err
	}
	if err = reference.validateIntent(intent); err != nil {
		return StartupIntent{}, err
	}
	return intent, nil
}

func requireContactPin(ctx context.Context, store endpointRootObserver, pin taskdir.RootPin) error {
	current, err := store.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !current.Equal(pin) {
		return ErrIdentity
	}
	return ctx.Err()
}

func matchContactSocket(socket unix.Stat_t, endpoint EndpointIdentity) error {
	if uint32(socket.Mode)&0o7777 != 0o600 || uint64(socket.Dev) != endpoint.SocketDev || uint64(socket.Ino) != endpoint.SocketInode {
		return ErrIdentity
	}
	return nil
}

func checkOwnerContactEndpoint(ctx context.Context, store *taskdir.ReadOnlyStore, pin taskdir.RootPin, endpoint EndpointIdentity) error {
	if err := requireContactPin(ctx, store, pin); err != nil {
		return err
	}
	socket, err := observeEndpointSocket(endpoint.Path)
	if err != nil {
		return err
	}
	if err = matchContactSocket(socket, endpoint); err != nil {
		return err
	}
	return ctx.Err()
}

func checkClosedOwnerContactEndpoint(ctx context.Context, store endpointRootObserver, pin taskdir.RootPin, endpoint EndpointIdentity) error {
	if err := requireContactPin(ctx, store, pin); err != nil {
		return err
	}
	socket, err := observeEndpointSocket(endpoint.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err = matchContactSocket(socket, endpoint); err != nil {
			return err
		}
	}
	return requireContactPin(ctx, store, pin)
}

// Cancellation wakes blocked socket I/O. The caller stops or joins the callback
// before closing its connection; the callback owns no store, lock or borrowed pin.
func watchOwnerContactCancellation(ctx context.Context, connection *net.UnixConn) func() error {
	done := make(chan struct{})
	var wakeErr error
	stop := context.AfterFunc(ctx, func() { wakeErr = connection.SetDeadline(time.Now()); close(done) })
	return func() error {
		if !stop() {
			<-done
		}
		return wakeErr
	}
}

// completeOwnerContact keeps an acknowledged result independent from late
// observation cancellation. Only a failed exchange needs the context-race cause.
func completeOwnerContact(verified bool, reply ownerReply, exchangeErr, contextErr, cleanupErr error) (ownerReply, error) {
	if exchangeErr != nil {
		exchangeErr = errors.Join(exchangeErr, contextErr)
	}
	resultErr := errors.Join(exchangeErr, cleanupErr)
	if resultErr == nil {
		return reply, nil
	}
	if !verified {
		reply = ownerReply{}
	}
	return reply, ownerContactError{cause: resultErr}
}

// Redact contact diagnostics while preserving all causes for classification.
type ownerContactError struct{ cause error }

func (ownerContactError) Error() string   { return "infrastructure owner contact failed" }
func (e ownerContactError) Unwrap() error { return e.cause }
