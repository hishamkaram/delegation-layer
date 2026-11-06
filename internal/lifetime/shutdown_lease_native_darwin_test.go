//go:build darwin

package lifetime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
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
	present, err := signalDarwinProcessWith(ctx, launch, signal, inspectDarwinProcessIdentityInfo, signalDarwinProcess)
	if err == nil && !present {
		return errProcessNoSuchEntity
	}
	return err
}

func shutdownLeaseDescriptors(ctx context.Context, pid int) (map[int]shutdownLeaseFileID, error) {
	executable, err := exec.LookPath("lsof")
	if err != nil {
		return nil, fmt.Errorf("native C3 proof requires lsof: %w", err)
	}
	data, err := runShutdownLeaseCommand(ctx, exec.Command(executable, "-nP", "-a", "-p", strconv.Itoa(pid), "-F", "fDin"))
	if err != nil {
		return nil, err
	}
	return parseShutdownLeaseLsof(string(data))
}

func parseShutdownLeaseLsof(data string) (map[int]shutdownLeaseFileID, error) {
	result := make(map[int]shutdownLeaseFileID)
	number, device, inode := -1, "", ""
	for _, line := range strings.Split(data+"\nfEND", "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'f':
			if number >= 0 && device != "" && inode != "" {
				identity, err := parseShutdownLeaseLsofIdentity(device, inode)
				if err != nil {
					return nil, err
				}
				result[number] = identity
			}
			value, err := strconv.Atoi(line[1:])
			if err != nil {
				number = -1
			} else {
				number = value
			}
			device, inode = "", ""
		case 'D':
			device = line[1:]
		case 'i':
			inode = line[1:]
		}
	}
	return result, nil
}

func parseShutdownLeaseLsofIdentity(device, inode string) (shutdownLeaseFileID, error) {
	dev, err := strconv.ParseUint(device, 0, 64)
	if err != nil {
		return shutdownLeaseFileID{}, err
	}
	ino, err := strconv.ParseUint(inode, 10, 64)
	if err != nil {
		return shutdownLeaseFileID{}, err
	}
	return shutdownLeaseFileID{Device: dev, Inode: ino}, nil
}

func shutdownLeasePeer(connection *net.UnixConn, pid int) error {
	raw, err := connection.SyscallConn()
	if err != nil {
		return err
	}
	var peer int
	var credential *unix.Xucred
	var socketErr error
	controlErr := raw.Control(func(fd uintptr) {
		var pidErr, credErr error
		peer, pidErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		credential, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		socketErr = errors.Join(pidErr, credErr)
	})
	if err = errors.Join(controlErr, socketErr); err != nil {
		return err
	}
	if peer != pid || credential == nil || credential.Version != 0 || credential.Uid != uint32(os.Geteuid()) {
		return errors.New("Darwin gate peer differs from exact owned CLI")
	}
	return nil
}

type shutdownLeaseExitWatch struct {
	fd       int
	observed bool
}

func newShutdownLeaseExitWatch(ref ProcessRef) (*shutdownLeaseExitWatch, error) {
	fd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	event := unix.Kevent_t{Ident: uint64(ref.PID()), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	_, err = unix.Kevent(fd, []unix.Kevent_t{event}, nil, nil)
	if err != nil {
		return nil, errors.Join(err, unix.Close(fd))
	}
	present, observeErr := ProcessPresent(ref)
	if observeErr != nil || !present {
		return nil, errors.Join(errors.New("actor changed before kqueue registration"), observeErr, unix.Close(fd))
	}
	return &shutdownLeaseExitWatch{fd: fd}, nil
}

func (w *shutdownLeaseExitWatch) wait(ctx context.Context) error {
	return w.waitWithKevent(ctx, unix.Kevent)
}

func (w *shutdownLeaseExitWatch) waitWithKevent(ctx context.Context, keventCall func(int, []unix.Kevent_t, []unix.Kevent_t, *unix.Timespec) (int, error)) error {
	return waitForEmpty(ctx, 10*time.Millisecond, func() (bool, error) {
		if w.observed {
			return true, nil
		}
		var event [1]unix.Kevent_t
		n, err := keventCall(w.fd, nil, event[:], &unix.Timespec{})
		if n <= 0 {
			if shutdownLeaseBareInterrupt(err) {
				return false, nil
			}
			if err != nil {
				return false, fmt.Errorf("kqueue exit wait fd=%d count=%d: %w", w.fd, n, err)
			}
			if n < 0 {
				return false, fmt.Errorf("unexpected kqueue event count %d", n)
			}
			return false, nil
		}
		if n != 1 || event[0].Flags&unix.EV_ERROR != 0 || event[0].Fflags&unix.NOTE_EXIT == 0 {
			return false, errors.Join(errors.New("unexpected native process exit event"), err)
		}
		// EV_ONESHOT was consumed. Latch the validated event even if an impossible
		// positive-count/error pair is returned; retain that fault, never retry it.
		w.observed = true
		if err != nil {
			return false, fmt.Errorf("kqueue delivered exit event with error: %w", err)
		}
		return true, nil
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

func TestShutdownLeaseKeventInterruptedWait(t *testing.T) {
	watch := &shutdownLeaseExitWatch{fd: 123}
	calls := 0
	err := watch.waitWithKevent(t.Context(), func(fd int, changes, events []unix.Kevent_t, timeout *unix.Timespec) (int, error) {
		calls++
		if fd != 123 || len(changes) != 0 || len(events) != 1 || timeout == nil || *timeout != (unix.Timespec{}) {
			t.Fatal("wait changed handle/registration/deadline")
		}
		if calls == 1 {
			return -1, unix.EINTR
		}
		events[0].Fflags = unix.NOTE_EXIT
		return 1, nil
	})
	if err != nil || calls != 2 || !watch.observed {
		t.Fatalf("interrupted same-handle wait calls=%d observed=%t err=%v", calls, watch.observed, err)
	}
}

func TestShutdownLeaseKeventInterruptRespectsOriginalContext(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			defer cancel()
			watch := &shutdownLeaseExitWatch{fd: 123}
			calls := 0
			err := watch.waitWithKevent(ctx, func(int, []unix.Kevent_t, []unix.Kevent_t, *unix.Timespec) (int, error) {
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

func TestShutdownLeaseKeventPreservesFaultsAndLatchesEvent(t *testing.T) {
	for _, test := range []struct {
		name         string
		count        int
		fault        error
		wantObserved bool
	}{
		{name: "native error", count: -1, fault: unix.EBADF},
		{name: "positive count and interrupt", count: 1, fault: unix.EINTR, wantObserved: true},
		{name: "joined error", count: -1, fault: errors.Join(unix.EINTR, unix.EBADF)},
		{name: "invalid event with fault", count: 1, fault: unix.EBADF},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			watch := &shutdownLeaseExitWatch{fd: 123}
			err := watch.waitWithKevent(t.Context(), func(_ int, _ []unix.Kevent_t, events []unix.Kevent_t, _ *unix.Timespec) (int, error) {
				calls++
				if test.wantObserved {
					events[0].Fflags = unix.NOTE_EXIT
				}
				return test.count, test.fault
			})
			if calls != 1 || !errors.Is(err, test.fault) || watch.observed != test.wantObserved {
				t.Fatalf("fault retried/lost event: calls=%d observed=%t err=%v", calls, watch.observed, err)
			}
		})
	}
}

func TestShutdownLeaseLsofRegularDescriptorRecords(t *testing.T) {
	observed, err := parseShutdownLeaseLsof("p123\nf3\nD0x1000005\ni42\nf4\nnpipe\nf5\nD0x1000005\ni43\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || observed[3] != (shutdownLeaseFileID{Device: 0x1000005, Inode: 42}) || observed[5] != (shutdownLeaseFileID{Device: 0x1000005, Inode: 43}) {
		t.Fatalf("regular descriptor identities changed: %+v", observed)
	}
	for _, invalid := range []string{"f3\nDinvalid\ni42\n", "f3\nD0x1000005\niinvalid\n"} {
		if _, parseErr := parseShutdownLeaseLsof(invalid); parseErr == nil {
			t.Fatal("invalid completed descriptor identity accepted")
		}
	}
}
