package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

// endpointError keeps native classification available without rendering a
// caller-selected pathname, including the address embedded in net.OpError.
type endpointError struct {
	operation string
	pathBytes int
	cause     error
}

func (e *endpointError) Error() string {
	return fmt.Sprintf("infrastructure endpoint %s failed (path bytes=%d)", e.operation, e.pathBytes)
}
func (e *endpointError) Unwrap() error  { return e.cause }
func (e *endpointError) PathBytes() int { return e.pathBytes }

// ownedEndpoint owns the listener and identity-bound socket cleanup. Its Store
// is borrowed and must outlive cleanup. The caller keeps the protected parent
// stable through supported root lifecycle operations. Path identity checks do
// not make chmod/unlink atomic against malicious same-UID replacement.
// This resource is neither an RPC server nor daemon-start authority.
type ownedEndpoint struct {
	mu               sync.Mutex
	listener         *net.UnixListener
	store            endpointRootObserver
	pin              taskdir.RootPin
	identity         EndpointIdentity
	listenerClosed   bool
	listenerCloseErr error
	removed          bool
}

// The endpoint needs only fresh root observations. Production always borrows
// its actual Store; this port supplies no lease, pin value or authority.
type endpointRootObserver interface {
	PinRootContext(context.Context) (taskdir.RootPin, error)
}

func (e *ownedEndpoint) Identity() EndpointIdentity { return e.identity }

// listenEndpoint binds only an initially absent target endpoint. Residue of any
// kind is refused; proving safe recovery belongs to the future coordinator.
// Any nonnil result transfers cleanup ownership, even when err is nonnil.
func (j *intentJournal) listenEndpoint(ctx context.Context, lease *Lease, reference StartupReference) (*ownedEndpoint, error) {
	if j == nil {
		return nil, &endpointError{operation: "listen", cause: os.ErrInvalid}
	}
	return j.listenEndpointWithObserver(ctx, lease, reference, j.store)
}

// Any nonnil result transfers cleanup ownership, even when err is nonnil.
func (j *intentJournal) listenEndpointWithObserver(ctx context.Context, lease *Lease, reference StartupReference, observer endpointRootObserver) (endpoint *ownedEndpoint, resultErr error) {
	resultErr = j.withOpen(ctx, func() error {
		if lease == nil || lease.path != filepath.Join(j.pin.Root(), instanceLeaseName) {
			return ErrIdentity
		}
		return lease.WithHeld(ctx, func() error {
			if err := j.validateEndpointAssumption(ctx, reference); err != nil {
				return err
			}
			var err error
			endpoint, err = bindInitialEndpoint(ctx, observer, j.pin)
			return err
		})
	})
	if resultErr != nil {
		size := 0
		if j != nil {
			size = len(filepath.Join(j.pin.Root(), ".i"))
		}
		return endpoint, &endpointError{operation: "listen", pathBytes: size, cause: resultErr}
	}
	return endpoint, nil
}

// Called within the caller's one journal/lease guard; this validates evidence,
// and creates no authority token or recursive resource borrow.
func (j *intentJournal) validateEndpointAssumption(ctx context.Context, reference StartupReference) error {
	if err := j.checkRoot(ctx); err != nil {
		return err
	}
	reader := controlOwnerReader{controlIntentReader{j.control}}
	intent, err := readReferencedIntent(ctx, reader, j.pin, reference)
	if err != nil {
		return err
	}
	if err = validateCurrentIntent(intent, j.pin, j.bootID); err != nil {
		return err
	}
	assumption, err := readAssumptionRecord(ctx, reader, j.pin, reference)
	if err != nil {
		return err
	}
	return validateSelfAssumption(assumption)
}

// This budget bounds resource cleanup, never renews a startup/task deadline.
const endpointCleanupTimeout = 5 * time.Second

func bindInitialEndpoint(ctx context.Context, store endpointRootObserver, pin taskdir.RootPin) (*ownedEndpoint, error) {
	path := filepath.Join(pin.Root(), ".i")
	var existing unix.Stat_t
	if err := unix.Lstat(path, &existing); err == nil {
		return nil, fmt.Errorf("%w: initial endpoint is occupied", os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	endpoint := &ownedEndpoint{listener: listener, store: store, pin: pin, identity: EndpointIdentity{Path: path, DirectoryDev: pin.Device(), DirectoryInode: pin.Inode()}}
	socket, err := observeEndpointSocket(path)
	if err != nil {
		return finalizeFailedEndpointBirth(ctx, endpoint, err)
	}
	endpoint.identity.SocketDev, endpoint.identity.SocketInode = uint64(socket.Dev), uint64(socket.Ino)
	if err = endpoint.protectSocket(ctx); err != nil {
		return finalizeFailedEndpointBirth(ctx, endpoint, err)
	}
	return endpoint, nil
}

func finalizeFailedEndpointBirth(ctx context.Context, endpoint *ownedEndpoint, birthErr error) (*ownedEndpoint, error) {
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), endpointCleanupTimeout)
	defer cancelCleanup()
	cleanupErr := endpoint.closeContext(cleanupCtx)
	if cleanupErr != nil {
		return endpoint, errors.Join(birthErr, cleanupErr)
	}
	return nil, birthErr
}

func observeEndpointSocket(path string) (unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return stat, err
	}
	if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFSOCK || stat.Ino == 0 || int64(stat.Uid) != int64(os.Geteuid()) {
		return stat, ErrIdentity
	}
	return stat, nil
}

func (e *ownedEndpoint) checkRoot(ctx context.Context) error {
	pin, err := e.store.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !pin.Equal(e.pin) {
		return ErrIdentity
	}
	return nil
}

func (e *ownedEndpoint) sameSocket(stat unix.Stat_t) bool {
	return uint64(stat.Dev) == e.identity.SocketDev && uint64(stat.Ino) == e.identity.SocketInode
}

func (e *ownedEndpoint) protectSocket(ctx context.Context) error {
	if err := e.checkRoot(ctx); err != nil {
		return err
	}
	socket, err := observeEndpointSocket(e.identity.Path)
	if err != nil {
		return err
	}
	if !e.sameSocket(socket) {
		return ErrIdentity
	}
	// Portable native socket APIs expose no descriptor for chmod of the bound
	// filesystem inode. Check its identity immediately before and after chmod;
	// the protected-parent lifecycle assumption above remains required.
	if uint32(socket.Mode)&0o7777 != 0o600 {
		if err = os.Chmod(e.identity.Path, 0o600); err != nil {
			return err
		}
	}
	socket, err = observeEndpointSocket(e.identity.Path)
	if err != nil {
		return err
	}
	if !e.sameSocket(socket) || uint32(socket.Mode)&0o7777 != 0o600 {
		return ErrIdentity
	}
	return errors.Join(e.checkRoot(ctx), ctx.Err())
}

// Close always closes the listener first. Socket cleanup succeeds only after
// current root and socket identity checks. On refusal it retains those facts
// for a bounded retry after the original root/path is restored; it never
// removes a replacement. Mode controls protection, not ownership; cleanup uses
// root/socket native identity, socket type and current UID. Listener closure
// alone is not cleanup success.
func (e *ownedEndpoint) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), endpointCleanupTimeout)
	defer cancel()
	return e.closeContext(ctx)
}

func (e *ownedEndpoint) closeContext(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.listenerClosed {
		e.listenerCloseErr = e.listener.Close()
		e.listenerClosed = true
	}
	if e.removed {
		return e.closeError(e.listenerCloseErr)
	}
	cleanupErr := e.removeSocket(ctx)
	if cleanupErr != nil {
		return e.closeError(errors.Join(e.listenerCloseErr, cleanupErr))
	}
	e.removed = true
	return e.closeError(e.listenerCloseErr)
}

func (e *ownedEndpoint) removeSocket(ctx context.Context) error {
	if err := e.checkRoot(ctx); err != nil {
		return err
	}
	socket, err := observeEndpointSocket(e.identity.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !e.sameSocket(socket) {
		return ErrIdentity
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = unix.Unlink(e.identity.Path); err != nil {
		return err
	}
	if err = e.checkRoot(ctx); err != nil {
		return err
	}
	var visible unix.Stat_t
	if err = unix.Lstat(e.identity.Path, &visible); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrIdentity, err)
	}
	return nil
}

func (e *ownedEndpoint) closeError(err error) error {
	if err == nil {
		return nil
	}
	return &endpointError{operation: "close", pathBytes: len(e.identity.Path), cause: err}
}
