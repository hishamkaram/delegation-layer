//go:build linux

package lifetime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func shutdownLeaseSignal(ctx context.Context, ref ProcessRef, signal syscall.Signal) error {
	group, err := processGroupID(ref.PID())
	if err != nil {
		return err
	}
	launch, err := NewLaunchRef(ref.PID(), ref.StartToken(), group)
	if err != nil {
		return err
	}
	return signalVerifiedProcessGroup(ctx, nil, group, []LaunchRef{launch}, signal)
}

func shutdownLeaseDescriptors(_ context.Context, pid int) (map[int]shutdownLeaseFileID, error) {
	base := "/proc/" + strconv.Itoa(pid) + "/fd"
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	result := make(map[int]shutdownLeaseFileID)
	for _, entry := range entries {
		fd, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			return nil, parseErr
		}
		var stat unix.Stat_t
		if err = unix.Stat(filepath.Join(base, entry.Name()), &stat); errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result[fd] = shutdownLeaseFileID{Device: uint64(stat.Dev), Inode: stat.Ino}
	}
	return result, nil
}

func shutdownLeasePeer(connection *net.UnixConn, pid int) error {
	raw, err := connection.SyscallConn()
	if err != nil {
		return err
	}
	var peer *unix.Ucred
	var socketErr error
	controlErr := raw.Control(func(fd uintptr) { peer, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if err = errors.Join(controlErr, socketErr); err != nil {
		return err
	}
	if peer == nil || int(peer.Pid) != pid || peer.Uid != uint32(os.Geteuid()) {
		return errors.New("native gate peer differs from exact owned CLI")
	}
	return nil
}

type shutdownLeaseExitWatch struct{ fd int }

func newShutdownLeaseExitWatch(ref ProcessRef) (*shutdownLeaseExitWatch, error) {
	fd, err := unix.PidfdOpen(ref.PID(), 0)
	if err != nil {
		return nil, err
	}
	present, observeErr := ProcessPresent(ref)
	if observeErr != nil || !present {
		return nil, errors.Join(errors.New("actor changed before pidfd registration"), observeErr, unix.Close(fd))
	}
	return &shutdownLeaseExitWatch{fd: fd}, nil
}

func (w *shutdownLeaseExitWatch) wait(ctx context.Context) error {
	return w.waitWithPoll(ctx, unix.Poll)
}

func (w *shutdownLeaseExitWatch) waitWithPoll(ctx context.Context, pollCall func([]unix.PollFd, int) (int, error)) error {
	return waitForEmpty(ctx, 10*time.Millisecond, func() (bool, error) {
		poll := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
		n, err := pollCall(poll, 0)
		// EINTR reports no event. Keep the SAME pidfd and original context; the
		// existing cancellable polling loop owns the next observation.
		if n <= 0 && shutdownLeaseBareInterrupt(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("pidfd exit Poll fd=%d count=%d: %w", w.fd, n, err)
		}
		if n < 0 || n > len(poll) {
			return false, fmt.Errorf("unexpected pidfd event count %d", n)
		}
		if n > 0 && poll[0].Revents&unix.POLLIN == 0 {
			return false, fmt.Errorf("unexpected pidfd events %d", poll[0].Revents)
		}
		return n > 0, nil
	}, sleepContext)
}

// Only the native syscall's bare EINTR reports an interrupted, undelivered
// event here. Wrapped/joined errors retain their independent fault ownership.
func shutdownLeaseBareInterrupt(err error) bool {
	var native syscall.Errno
	if !errors.As(err, &native) || !errors.Is(native, unix.EINTR) || errors.Unwrap(err) != nil {
		return false
	}
	_, joined := err.(interface{ Unwrap() []error })
	return !joined
}

func (w *shutdownLeaseExitWatch) close() error { return unix.Close(w.fd) }

func TestShutdownLeasePollInterruptedWait(t *testing.T) {
	watch := &shutdownLeaseExitWatch{fd: 123}
	calls := 0
	err := watch.waitWithPoll(t.Context(), func(fds []unix.PollFd, timeout int) (int, error) {
		calls++
		if len(fds) != 1 || fds[0].Fd != 123 || timeout != 0 {
			t.Fatalf("wait retargeted/reset syscall: %+v timeout=%d", fds, timeout)
		}
		if calls == 1 {
			return -1, unix.EINTR
		}
		fds[0].Revents = unix.POLLIN
		return 1, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("interrupted same-handle wait calls=%d err=%v", calls, err)
	}
}

func TestShutdownLeasePollInterruptRespectsOriginalContext(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			defer cancel()
			watch := &shutdownLeaseExitWatch{fd: 123}
			calls := 0
			err := watch.waitWithPoll(ctx, func([]unix.PollFd, int) (int, error) {
				calls++
				if !deadline {
					cancel()
				}
				return -1, unix.EINTR
			})
			expected := context.Canceled
			if deadline {
				expected = context.DeadlineExceeded
			}
			if !errors.Is(err, expected) || calls == 0 {
				t.Fatalf("original context calls=%d err=%v, want %v", calls, err, expected)
			}
		})
	}
}

func TestShutdownLeasePollPreservesFaults(t *testing.T) {
	for _, test := range []struct {
		name  string
		count int
		fault error
	}{
		{name: "native error", count: -1, fault: unix.EBADF},
		{name: "positive count and interrupt", count: 1, fault: unix.EINTR},
		{name: "joined error", count: -1, fault: errors.Join(unix.EINTR, unix.EBADF)},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			watch := &shutdownLeaseExitWatch{fd: 123}
			err := watch.waitWithPoll(t.Context(), func([]unix.PollFd, int) (int, error) { calls++; return test.count, test.fault })
			if calls != 1 || !errors.Is(err, test.fault) {
				t.Fatalf("fault retried/lost calls=%d err=%v", calls, err)
			}
		})
	}
}
