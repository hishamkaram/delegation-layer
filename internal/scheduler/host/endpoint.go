package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// endpoint owns only its invocation-created directory and socket. The listener
// never unlinks on Close; pinned parent/object identity checks govern removal.
type endpoint struct {
	listener   *net.UnixListener
	path       string
	name       string
	parent     *os.Root
	directory  *os.Root
	dirInfo    os.FileInfo
	socketInfo os.FileInfo
	once       sync.Once
	closeErr   error
}

func newEndpoint() (*endpoint, error) {
	dir, err := os.MkdirTemp("", "dh-")
	if err != nil {
		return nil, err
	}
	e := &endpoint{name: filepath.Base(dir), path: filepath.Join(dir, "s")}
	e.parent, err = os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		// Without a pinned parent/object token, this path is not cleanup authority.
		return nil, err
	}
	e.dirInfo, err = e.parent.Lstat(e.name)
	if err != nil {
		return nil, errors.Join(err, e.parent.Close())
	}
	e.directory, err = e.parent.OpenRoot(e.name)
	if err != nil {
		return nil, errors.Join(err, e.close())
	}
	if len(e.path) >= len(unix.RawSockaddrUnix{}.Path) {
		return nil, errors.Join(fmt.Errorf("scheduler host temporary endpoint exceeds native Unix socket limit"), e.close())
	}
	e.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: e.path, Net: "unix"})
	if err != nil {
		return nil, errors.Join(err, e.close())
	}
	e.listener.SetUnlinkOnClose(false)
	e.socketInfo, err = e.directory.Lstat("s")
	if err != nil {
		return nil, errors.Join(err, e.close())
	}
	return e, nil
}

func (e *endpoint) close() error {
	e.once.Do(func() {
		if e.listener != nil {
			e.closeErr = e.listener.Close()
		}
		if e.directory != nil {
			if e.socketInfo != nil {
				current, err := e.directory.Lstat("s")
				if err == nil && os.SameFile(current, e.socketInfo) {
					err = e.directory.Remove("s")
				} else if err == nil {
					err = ErrIdentity
				}
				e.closeErr = errors.Join(e.closeErr, err)
			}
			e.closeErr = errors.Join(e.closeErr, e.directory.Close())
		}
		current, err := e.parent.Lstat(e.name)
		if err == nil && os.SameFile(current, e.dirInfo) {
			err = e.parent.Remove(e.name) // Refuses nonempty/replaced contents.
		} else if err == nil {
			err = ErrIdentity
		}
		e.closeErr = errors.Join(e.closeErr, err, e.parent.Close())
	})
	return e.closeErr
}

// accept owns one bounded handler at a time. Cancel closes the listener and
// the active connection; completion joins both before the H owner can return.
func (e *endpoint) accept(ctx context.Context, epoch epochRecord, requests chan<- controlRequest, done chan<- error) {
	var resultErr error
	for {
		conn, err := e.listener.AcceptUnix()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				resultErr = errors.Join(resultErr, err)
			}
			done <- resultErr
			return
		}
		if err = serveConnection(ctx, conn, epoch, requests); err != nil && ctx.Err() == nil && !errors.Is(err, ErrIdentity) && !lostReplyOnly(err) {
			// Invalid clients are refused, not authority to end the host. Retain
			// one bounded fault for the owner instead of accumulating requests.
			resultErr = errors.New("scheduler host refused a control connection")
		}
	}
}
