// Package infrastructure owns resident infrastructure resources.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

// Lease owns one descriptor reference to the resident instance flock. Only
// election or trusted inheritance establishes ownership; validation never locks.
// Its mutex serializes borrowed use and closure, not the resident's policy.
// Never unlink its lock file in a live root. Root disposal must first prove
// resident closure and a free instance lease, independently of this descriptor.
type Lease struct {
	mu    sync.Mutex
	file  *os.File
	path  string
	dev   uint64
	inode uint64
}

// TryAcquireLease attempts election once. A busy lease returns (nil, false, nil).
// The caller supplies an existing, protected private parent directory and keeps
// that directory stable. This operation creates only the lock file.
func TryAcquireLease(ctx context.Context, path string) (*Lease, bool, error) {
	if ctx == nil {
		return nil, false, os.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := validateParent(path); err != nil {
		return nil, false, err
	}
	lock, err := taskdir.OpenLockFileContext(ctx, path, taskdir.LockLevelNone)
	if err != nil {
		return nil, false, err
	}
	if err = lock.LockEXNonblockingContext(ctx); err != nil {
		closeErr := lock.Close()
		if errors.Is(err, task.ErrLockBusy) && closeErr == nil {
			return nil, false, nil
		}
		return nil, false, errors.Join(err, closeErr)
	}
	file, err := lock.TakeExclusiveFile()
	if err != nil {
		return nil, false, errors.Join(err, lock.Close())
	}
	lease, err := AdoptInheritedLease(file, path)
	return lease, err == nil, err
}

// AdoptInheritedLease consumes file, including on failure. The fixed trusted
// child entry must establish that it inherited an already-held exclusive flock
// from its trusted birth chain. Native descriptor identity is not root or
// startup authority, and this constructor cannot prove that a flock is held.
// It never acquires a flock. CLOEXEC is set before any other validation; callers
// must invoke this before spawning children or serving requests.
func AdoptInheritedLease(file *os.File, path string) (*Lease, error) {
	if file == nil {
		return nil, os.ErrInvalid
	}
	lease, err := adoptLease(file, path)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return lease, nil
}

func adoptLease(file *os.File, path string) (*Lease, error) {
	fd := file.Fd()
	flags, err := unix.FcntlInt(fd, unix.F_GETFD, 0)
	if err != nil {
		return nil, fmt.Errorf("read instance descriptor flags: %w", err)
	}
	if _, err = unix.FcntlInt(fd, unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return nil, fmt.Errorf("protect instance descriptor: %w", err)
	}
	if err = validateParent(path); err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err = unix.Fstat(int(fd), &stat); err != nil {
		return nil, err
	}
	lease := &Lease{file: file, path: path, dev: uint64(stat.Dev), inode: uint64(stat.Ino)}
	if err = lease.validate(); err != nil {
		return nil, err
	}
	return lease, nil
}

func validateParent(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("instance lease path must be clean and absolute: %w", os.ErrInvalid)
	}
	var stat unix.Stat_t
	if err := unix.Lstat(filepath.Dir(path), &stat); err != nil {
		return err
	}
	if taskdir.CheckPrivateStat(&stat, os.Geteuid(), true) != taskdir.PrivateStatAccepted {
		return fmt.Errorf("instance lease parent is not private: %w", task.ErrEvidenceFault)
	}
	return nil
}

func (l *Lease) validate() error {
	var descriptor, visible unix.Stat_t
	if err := unix.Fstat(int(l.file.Fd()), &descriptor); err != nil {
		return err
	}
	if err := unix.Lstat(l.path, &visible); err != nil {
		return err
	}
	if taskdir.CheckPrivateStat(&descriptor, os.Geteuid(), false) != taskdir.PrivateStatAccepted ||
		taskdir.CheckPrivateStat(&visible, os.Geteuid(), false) != taskdir.PrivateStatAccepted ||
		uint64(descriptor.Dev) != l.dev || uint64(descriptor.Ino) != l.inode ||
		uint64(visible.Dev) != l.dev || uint64(visible.Ino) != l.inode {
		return fmt.Errorf("instance lease identity changed: %w", task.ErrEvidenceFault)
	}
	flags, err := unix.FcntlInt(l.file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		return err
	}
	if flags&unix.FD_CLOEXEC == 0 {
		return fmt.Errorf("instance descriptor is not close-on-exec: %w", task.ErrEvidenceFault)
	}
	access, err := unix.FcntlInt(l.file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return err
	}
	if access&unix.O_ACCMODE != unix.O_RDWR {
		return fmt.Errorf("instance descriptor is not read-write: %w", task.ErrEvidenceFault)
	}
	return nil
}

// WithHeld performs a checked operation under the lease's closure guard without
// exposing its descriptor. Resident-authority validation and daemon Start use
// this boundary. The callback must not reenter this lease or call Close.
// This guard alone cannot establish trusted inherited ownership.
func (l *Lease) WithHeld(ctx context.Context, callback func() error) error {
	if callback == nil {
		return os.ErrInvalid
	}
	return l.WithHeldFile(ctx, func(*os.File) error { return callback() })
}

// WithHeldFile borrows the checked descriptor only for explicit child-at-birth
// inheritance. Use WithHeld for other operations to prevent descriptor leaks.
// The callback must not close, unlock, replace, or retain the descriptor and must
// not call Close on this lease. Native Start and its authority checks belong
// together inside this callback. Cancellation is checked before borrowing;
// an active callback remains responsible for its own completion.
func (l *Lease) WithHeldFile(ctx context.Context, callback func(*os.File) error) error {
	if l == nil || ctx == nil || callback == nil {
		return os.ErrInvalid
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := l.validate(); err != nil {
		return err
	}
	return callback(l.file)
}

// Close drops only this descriptor reference, never LOCK_UN: inherited
// references must retain their flock. It waits for the active borrower.
func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	return file.Close()
}
