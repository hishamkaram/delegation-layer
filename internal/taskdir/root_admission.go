package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const rootAdmissionLockName = ".root-admission.lock"

// RootAdmissionLockName exposes the storage owner's canonical basename for
// native descriptor checks. The name alone grants no admission authority.
const RootAdmissionLockName = rootAdmissionLockName

type rootAdmissionMode uint8

const (
	rootAdmissionUnacquired rootAdmissionMode = iota
	rootAdmissionShared
	rootAdmissionExclusive
)

// RootAdmissionLease owns one native root-admission flock and its descriptor.
// Store is borrowed and must outlive Release. Application policy owns what work
// the SH/EX fence covers; this resource grants no native Start permission.
// Pass the held lease explicitly through nested admission instead of acquiring
// SH again while an EX waiter exists. Cancellation never releases a live lease.
// Never unlink this inode in a live root. Current identity observations are not
// atomic against malicious same-UID replacement or a substitute for finalization.
type RootAdmissionLease struct {
	mu                  sync.Mutex
	store               *Store
	pin                 RootPin
	lock                *LockFile
	maintenance         *LockFile // Opening only; never retained during admission waiting.
	closeErr            error
	mode                rootAdmissionMode
	file                *os.File // Consumed original SH descriptor; every holder closes only.
	duplicateCleanupErr error
	duplicateFile       func(*os.File) (*os.File, error) // Fixed native I/O; private fault seam.
}

// RetainsStore observes cleanup ownership, not launch permission. Only the
// original borrowed Store must stay open while this owner has unresolved
// resources. Another Store for the same root remains independently closable.
func (l *RootAdmissionLease) RetainsStore(store *Store) bool {
	if l == nil || store == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.store == store && l.cleanupPendingHeld()
}

// CleanupPending reads this owner's unresolved resources, without granting
// permission or retrying a failed native cleanup.
func (l *RootAdmissionLease) CleanupPending() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cleanupPendingHeld()
}

// The resource guard is held by both ownership views.
func (l *RootAdmissionLease) cleanupPendingHeld() bool {
	return l.lock != nil || l.file != nil || l.maintenance != nil || l.closeErr != nil || l.duplicateCleanupErr != nil
}

// AcquireRootAdmissionContext acquires SH on an existing Store, lazily creating
// only its fence inode. Any nonnil result, including on error, is caller-owned
// and requires Release; error handles are cleanup-only, never successful admission.
func (s *Store) AcquireRootAdmissionContext(ctx context.Context) (*RootAdmissionLease, error) {
	return s.acquireRootAdmissionContext(ctx, false)
}

// TryAcquireRootAdmissionExclusiveContext tries EX without waiting for either
// maintenance or admission contention. Conflicts preserve task.ErrLockBusy.
// It never initializes or creates an absent root. Nonnil error results require Release.
func (s *Store) TryAcquireRootAdmissionExclusiveContext(ctx context.Context) (*RootAdmissionLease, error) {
	return s.acquireRootAdmissionContext(ctx, true)
}

func (s *Store) acquireRootAdmissionContext(ctx context.Context, exclusive bool) (*RootAdmissionLease, error) {
	lease, err := s.openRootAdmissionContext(ctx, exclusive)
	if err != nil {
		return lease, err
	}
	if err = ctx.Err(); err == nil {
		if exclusive {
			err = lease.lock.LockEXNonblockingContext(ctx)
		} else {
			err = lease.lock.LockSH(ctx)
		}
	}
	if err == nil {
		lease.mu.Lock()
		err = lease.checkRootContextHeld(ctx, s)
		if err == nil {
			lease.mode = rootAdmissionShared
			if exclusive {
				lease.mode = rootAdmissionExclusive
			}
		}
		lease.mu.Unlock()
	}
	if err != nil {
		return failedRootAdmission(lease, err)
	}
	return lease, nil
}

// The maintenance hold protects only rooted inode opening/durability. Drop it
// before the first native admission attempt: Stop/finalization may hold root EX
// and need maintenance EX, so waiting for root SH under maintenance SH would cycle.
func (s *Store) openRootAdmissionContext(ctx context.Context, nonblocking bool) (*RootAdmissionLease, error) {
	if s == nil || ctx == nil || s.maintLock == nil || s.rootHandle == nil {
		return nil, os.ErrInvalid
	}
	pin, err := s.PinRootContext(ctx)
	if err != nil {
		return nil, err
	}
	if nonblocking {
		err = s.maintLock.LockSHNonblockingContext(ctx)
	} else {
		err = s.maintLock.LockSH(ctx)
	}
	if err != nil {
		return nil, err
	}
	lease := &RootAdmissionLease{store: s, pin: pin, maintenance: s.maintLock}
	if err = checkRootAdmissionPin(ctx, s, pin); err == nil {
		lease.lock, err = s.openLock(filepath.Join(pin.Root(), rootAdmissionLockName), LockLevelRootAdmission)
	}
	unlockErr := lease.maintenance.Unlock()
	if unlockErr == nil {
		lease.maintenance = nil
	}
	if err = errors.Join(err, unlockErr); err != nil {
		return failedRootAdmission(lease, err)
	}
	return lease, nil
}

func failedRootAdmission(lease *RootAdmissionLease, cause error) (*RootAdmissionLease, error) {
	if releaseErr := lease.Release(); releaseErr != nil {
		return lease, errors.Join(cause, releaseErr)
	}
	return nil, cause
}

func checkRootAdmissionPin(ctx context.Context, store *Store, expected RootPin) error {
	current, err := store.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !current.Equal(expected) {
		return task.ErrIdentityMismatch
	}
	return nil
}

// CheckRootContext checks a held descriptor, its canonical name and the actual
// original/expected Store pins. A separate Store for the same native root is
// accepted; pathname/RootID equality alone never substitutes for the full pin.
// This is an observation, not operation permission or an atomic pathname guard.
func (l *RootAdmissionLease) CheckRootContext(ctx context.Context, expectedStore *Store) error {
	if l == nil || ctx == nil || expectedStore == nil {
		return os.ErrInvalid
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mode == rootAdmissionUnacquired {
		return ErrLockNotHeld
	}
	return l.checkRootContextHeld(ctx, expectedStore)
}

// PinRootContext observes the actual held SH resource through its original
// borrowed Store. Consumers may bind a command to this immutable native pin;
// the observation neither duplicates the lease nor authorizes native Start.
func (l *RootAdmissionLease) PinRootContext(ctx context.Context) (RootPin, error) {
	if l == nil || ctx == nil {
		return RootPin{}, os.ErrInvalid
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mode != rootAdmissionShared {
		return RootPin{}, task.ErrInvalidPermit
	}
	if err := l.checkRootContextHeld(ctx, l.store); err != nil {
		return RootPin{}, err
	}
	return l.pin, nil
}

// Called under the resource guard, including the final pre-mint acquisition check.
func (l *RootAdmissionLease) checkRootContextHeld(ctx context.Context, expectedStore *Store) error {
	if err := errors.Join(l.closeErr, l.duplicateCleanupErr); err != nil {
		return err
	}
	if l.lock == nil {
		return os.ErrClosed
	}
	if l.maintenance != nil {
		return ErrLockNotHeld
	}
	if err := checkRootAdmissionPin(ctx, l.store, l.pin); err != nil {
		return err
	}
	if expectedStore != l.store {
		if err := checkRootAdmissionPin(ctx, expectedStore, l.pin); err != nil {
			return err
		}
	}
	if err := l.checkRootAdmissionInode(ctx); err != nil {
		return err
	}
	return checkRootAdmissionPin(ctx, l.store, l.pin)
}

func (l *RootAdmissionLease) checkRootAdmissionInode(ctx context.Context) error {
	lock := l.lock
	lock.mu.Lock()
	defer lock.mu.Unlock()
	fd, err := l.activeAdmissionFDHeld()
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	var descriptor, visible unix.Stat_t
	if err = unix.Fstat(fd, &descriptor); err != nil {
		return err
	}
	// Opening is rooted. Fresh original-Store pins before and after this native
	// name observation bind it to the same root, without creating another owner.
	if err = unix.Lstat(filepath.Join(l.pin.Root(), rootAdmissionLockName), &visible); err != nil {
		return errors.Join(task.ErrIdentityMismatch, err)
	}
	if CheckPrivateStat(&descriptor, os.Geteuid(), false) != PrivateStatAccepted || CheckPrivateStat(&visible, os.Geteuid(), false) != PrivateStatAccepted {
		return task.ErrEvidenceFault
	}
	if uint64(descriptor.Dev) != lock.dev || uint64(descriptor.Ino) != lock.inode || uint64(visible.Dev) != lock.dev || uint64(visible.Ino) != lock.inode {
		return task.ErrIdentityMismatch
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return err
	}
	if flags&unix.FD_CLOEXEC == 0 {
		return task.ErrEvidenceFault
	}
	return ctx.Err()
}

// Both resource and LockFile guards are held. A consumed LockFile is closed,
// while the resource's transferred file remains the actual held reference.
func (l *RootAdmissionLease) activeAdmissionFDHeld() (int, error) {
	if l.file != nil {
		return int(l.file.Fd()), nil
	}
	if l.lock.closed {
		return -1, os.ErrClosed
	}
	if !l.lock.isEx && l.lock.shCount == 0 {
		return -1, ErrLockNotHeld
	}
	return l.lock.fd, nil
}

// Release unlocks/closes only this resource, never the pathname. Failed unlocks
// retain ownership for retry. Failed native Close remains unproved and is
// reported on future calls; an already-invalid wrapper cannot certify cleanup.
// The opening maintenance hold, if any, is independently released as well.
func (l *RootAdmissionLease) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	lockErr := l.closeErr
	if l.file != nil && lockErr == nil {
		lockErr = l.file.Close()
		if lockErr == nil {
			l.file, l.lock = nil, nil
		} else {
			l.closeErr = lockErr
		}
	} else if l.lock != nil && lockErr == nil {
		lockErr = l.lock.Close()
		if lockErr == nil {
			l.lock = nil
		} else {
			l.lock.mu.Lock()
			if l.lock.closed {
				l.closeErr = lockErr
			}
			l.lock.mu.Unlock()
		}
	}
	var maintenanceErr error
	if l.maintenance != nil {
		maintenanceErr = l.maintenance.Unlock()
		if maintenanceErr == nil {
			l.maintenance = nil
		}
	}
	return errors.Join(lockErr, maintenanceErr, l.duplicateCleanupErr)
}

// DuplicateControlLeaseFiles structurally supplies one inherited copy of an
// already minted SH admission hold. EX and cleanup-only handles are never
// forwardable. No descriptor, name or Options flag creates new admission.
// The existing transport has no context parameter: these synchronous native
// identity checks use Background, never a retained acquisition context. Context
// cancellation does not release caller-owned work; consumers own launch timing.
// Store must outlive this parent's Release. Returned copies own only their file
// reference and must close without LOCK_UN, including after further inheritance.
func (l *RootAdmissionLease) DuplicateControlLeaseFiles() ([]*os.File, error) {
	if l == nil {
		return nil, os.ErrInvalid
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mode != rootAdmissionShared {
		return nil, task.ErrInvalidPermit
	}
	if err := l.checkRootContextHeld(context.Background(), l.store); err != nil {
		return nil, err
	}
	if l.file == nil {
		file, err := l.lock.takeSharedFile()
		if err != nil {
			return nil, err
		}
		l.file = file
	}
	duplicate := duplicateRootAdmissionFile
	if l.duplicateFile != nil {
		duplicate = l.duplicateFile
	}
	file, err := duplicate(l.file)
	if err != nil {
		return nil, l.finishDuplicateFailure(file, err)
	}
	if file == nil {
		return nil, os.ErrInvalid
	}
	if err = l.checkRootContextHeld(context.Background(), l.store); err != nil {
		return nil, l.finishDuplicateFailure(file, err)
	}
	if err = l.checkDuplicate(file); err != nil {
		return nil, l.finishDuplicateFailure(file, err)
	}
	return []*os.File{file}, nil
}

func duplicateRootAdmissionFile(file *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	duplicate := os.NewFile(uintptr(fd), "delegate-root-admission-sh")
	if duplicate == nil {
		return nil, errors.Join(os.ErrInvalid, unix.Close(fd))
	}
	return duplicate, nil
}

func (l *RootAdmissionLease) checkDuplicate(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if CheckPrivateStat(&stat, os.Geteuid(), false) != PrivateStatAccepted || uint64(stat.Dev) != l.lock.dev || uint64(stat.Ino) != l.lock.inode {
		return task.ErrIdentityMismatch
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		return err
	}
	if flags&unix.FD_CLOEXEC == 0 {
		return task.ErrEvidenceFault
	}
	return nil
}

// The structural transport ignores files returned alongside an error. Failed
// copies remain this resource's cleanup responsibility, never the transport's.
func (l *RootAdmissionLease) finishDuplicateFailure(file *os.File, cause error) error {
	if file != nil {
		closeErr := file.Close()
		l.duplicateCleanupErr = errors.Join(l.duplicateCleanupErr, closeErr)
		cause = errors.Join(cause, closeErr)
	}
	return cause
}
