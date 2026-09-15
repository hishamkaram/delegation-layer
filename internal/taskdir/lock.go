package taskdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

var (
	ErrLockOrderViolation = errors.New("lock order violation: lock acquisition must follow maintenance -> session -> admission OR run")
	ErrLockNotHeld        = errors.New("lock is not held")
)

// LockLevel specifies hierarchy for lock acquisition.
type LockLevel int

const (
	LockLevelNone        LockLevel = 0
	LockLevelMaintenance LockLevel = 1
	LockLevelSession     LockLevel = 2
	LockLevelAdmission   LockLevel = 3
	LockLevelRun         LockLevel = 4
)

// LockFile represents a stable advisory flock file on a dedicated inode.
// It manages reference-counted shared leases and exclusive locks to prevent
// one completed operation on a shared Store from releasing flock protection for another.
type LockFile struct {
	path        string
	level       LockLevel
	file        *os.File
	fd          int
	inode       uint64
	dev         uint64
	mu          sync.Mutex
	shCount     int
	isEx        bool
	closed      bool
	faultSource func() FaultInjector
}

// OpenLockFile opens or creates a lock file with O_CLOEXEC, mode 0600,
// syncs the lock file AND its parent directory, and verifies FD_CLOEXEC.
func OpenLockFile(path string, level LockLevel) (*LockFile, error) {
	dir := filepath.Dir(path)
	if err := createDirectoriesWithAncestorBarrier(dir, nil); err != nil {
		return nil, fmt.Errorf("creating lock parent directory %s: %w", dir, err)
	}

	f, err := openLockInode(func(flags int) (*os.File, error) {
		fd, openErr := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
		if openErr != nil {
			return nil, openErr
		}
		return os.NewFile(uintptr(fd), path), nil
	}, true)
	if err != nil {
		return nil, err
	}
	return finishLockOpen(f, path, level, func() error { return platformBarrierDir(dir) }, nil)
}

func (s *Store) openLock(path string, level LockLevel) (*LockFile, error) {
	return s.openRootedLock(path, level, true)
}

func (s *Store) openExistingLock(path string, level LockLevel) (*LockFile, error) {
	return s.openRootedLock(path, level, false)
}

func (s *Store) openRootedLock(path string, level LockLevel, create bool) (*LockFile, error) {
	f, err := openLockInode(func(flags int) (*os.File, error) {
		return s.openFile(path, flags)
	}, create)
	if err != nil {
		return nil, err
	}
	lock, err := finishLockOpen(f, path, level, func() error { return s.barrierDir(filepath.Dir(path)) }, s.faultInjector)
	if err == nil {
		lock.faultSource = s.FaultInjector
	}
	return lock, err
}

// openLockInode creates a stable lock inode exactly once, or opens the existing
// winner. Concurrent nonexclusive O_CREAT opens can return ENOENT on Darwin.
// Only EEXIST from exclusive creation permits opening an existing inode; an
// absent lock on an existing-only path is never recreated or retried.
func openLockInode(open func(int) (*os.File, error), create bool) (*os.File, error) {
	if !create {
		return open(unix.O_RDWR)
	}
	f, err := open(unix.O_CREAT | unix.O_EXCL | unix.O_RDWR)
	if errors.Is(err, os.ErrExist) {
		return open(unix.O_RDWR)
	}
	return f, err
}

func finishLockOpen(f *os.File, path string, level LockLevel, parentBarrier func() error, injector FaultInjector) (*LockFile, error) {
	if err := validatePrivateFile(f, false); err != nil {
		closeFileQuietly(f)
		return nil, err
	}
	fd := int(f.Fd())
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		closeFileQuietly(f)
		return nil, fmt.Errorf("checking FD_CLOEXEC on %s: %w", path, err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		closeFileQuietly(f)
		return nil, fmt.Errorf("lock fd missing FD_CLOEXEC: %s", path)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		closeFileQuietly(f)
		return nil, fmt.Errorf("stat lock fd %s: %w", path, err)
	}

	if hooks, ok := injector.(LockFaultInjector); ok {
		if err := hooks.OnLockFileBarrier(path); err != nil {
			closeFileQuietly(f)
			return nil, err
		}
	}
	if err := platformBarrierFile(f); err != nil {
		closeFileQuietly(f)
		return nil, fmt.Errorf("syncing initial lock file %s: %w", path, err)
	}
	if hooks, ok := injector.(LockFaultInjector); ok {
		if err := hooks.OnLockParentDirBarrier(path); err != nil {
			closeFileQuietly(f)
			return nil, err
		}
	}
	if err := parentBarrier(); err != nil {
		closeFileQuietly(f)
		return nil, fmt.Errorf("syncing lock parent directory %s: %w", filepath.Dir(path), err)
	}

	return &LockFile{
		path:  path,
		level: level,
		file:  f,
		fd:    fd,
		inode: uint64(stat.Ino),
		dev:   uint64(stat.Dev),
	}, nil
}

// Inode returns the recorded filesystem inode for the lock file.
func (l *LockFile) Inode() uint64 {
	return l.inode
}

// Fd returns the underlying file descriptor.
func (l *LockFile) Fd() int {
	return l.fd
}

func (l *LockFile) waitLockBusy(ctx context.Context) error {
	if ctx == nil {
		return task.ErrLockBusy
	}
	l.mu.Unlock()
	defer l.mu.Lock()
	select {
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", task.ErrLockBusy, ctx.Err())
	case <-time.After(10 * time.Millisecond):
		return nil
	}
}

func (l *LockFile) flockUnlock() error {
	for {
		err := l.flock(unix.LOCK_UN)
		if err == nil {
			return nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}

// LockSHNonblocking attempts to acquire a shared lock without blocking.
func (l *LockFile) LockSHNonblocking() error {
	return l.LockSH(nil) //nolint:staticcheck // nonblocking entrypoint purposefully passes nil context
}

// LockEXNonblocking attempts to acquire an exclusive lock without blocking.
func (l *LockFile) LockEXNonblocking() error {
	return l.LockEX(nil) //nolint:staticcheck // nonblocking entrypoint purposefully passes nil context
}

// LockSH acquires a shared advisory lease. If another shared lease is already held on this descriptor,
// it increments the local lease count without calling unlock.
func (l *LockFile) LockSH(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for {
		if l.closed {
			return os.ErrClosed
		}
		if l.isEx {
			if err := l.waitLockBusy(ctx); err != nil {
				return err
			}
			continue
		}

		if l.shCount > 0 {
			l.shCount++
			return nil
		}

		// First shared lease on this descriptor: acquire kernel flock
		err := l.flock(unix.LOCK_SH | unix.LOCK_NB)
		if err == nil {
			l.shCount = 1
			return nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			if wErr := l.waitLockBusy(ctx); wErr != nil {
				return wErr
			}
			continue
		}
		return fmt.Errorf("flock SH %s: %w", l.path, err)
	}
}

// LockEX acquires an exclusive advisory lock. It will not upgrade an active shared lease
// and fails with ErrLockBusy if ctx is nil (nonblocking) or when timed out.
func (l *LockFile) LockEX(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for {
		if l.closed {
			return os.ErrClosed
		}
		if l.shCount > 0 || l.isEx {
			if err := l.waitLockBusy(ctx); err != nil {
				return err
			}
			continue
		}

		err := l.flock(unix.LOCK_EX | unix.LOCK_NB)
		if err == nil {
			l.isEx = true
			return nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			if wErr := l.waitLockBusy(ctx); wErr != nil {
				return wErr
			}
			continue
		}
		return fmt.Errorf("flock EX %s: %w", l.path, err)
	}
}

// Unlock decrements a shared lease or releases an exclusive lock.
// The kernel flock is only released when all shared leases have been released.
func (l *LockFile) Unlock() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.isEx {
		if err := l.flockUnlock(); err != nil {
			return fmt.Errorf("unlock EX %s: %w", l.path, err)
		}
		l.isEx = false
		return nil
	}

	if l.shCount > 0 {
		l.shCount--
		if l.shCount == 0 {
			if err := l.flockUnlock(); err != nil {
				return fmt.Errorf("unlock SH %s: %w", l.path, err)
			}
		}
	}

	return nil
}

// Close releases any held lock and closes the lock file descriptor without unlinking the file.
func (l *LockFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.isEx || l.shCount > 0 {
		if err := l.flockUnlock(); err != nil {
			return fmt.Errorf("unlock on close %s: %w", l.path, err)
		}
		l.isEx = false
		l.shCount = 0
	}
	if l.closed {
		return nil
	}
	l.closed = true
	return l.file.Close()
}

// LockFaultInjector supplies deterministic syscall failures to lock unit tests.
// All successful operations still execute the actual kernel primitive.
type LockFaultInjector interface {
	OnLockFileBarrier(path string) error
	OnLockParentDirBarrier(path string) error
	OnFlock(path string, operation int) error
}

func (l *LockFile) flock(operation int) error {
	if l.faultSource != nil {
		if hooks, ok := l.faultSource().(LockFaultInjector); ok {
			if err := hooks.OnFlock(l.path, operation); err != nil {
				return err
			}
		}
	}
	return unix.Flock(l.fd, operation)
}

func (l *LockFile) closeIdle() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	if l.isEx || l.shCount != 0 {
		return task.ErrLockBusy
	}
	l.closed = true
	return l.file.Close()
}
