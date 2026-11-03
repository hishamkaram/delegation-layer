package taskdir

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// RootPin is an immutable observation of validated store metadata and its
// native directory identity. It is not permission or start authority. Callers
// must revalidate against the current owner before use; the snapshot does not
// make later pathname operations atomic or protect against malicious same-UID
// replacement. Reobserve the full RootID/device/inode binding before use:
// inode reuse after deletion means this is not a permanent global identity.
type RootPin struct {
	root   string
	rootID string
	device uint64
	inode  uint64
}

func (p RootPin) Root() string   { return p.root }
func (p RootPin) RootID() string { return p.rootID }
func (p RootPin) Device() uint64 { return p.device }
func (p RootPin) Inode() uint64  { return p.inode }

// Equal compares the full observed value, including native identity.
func (p RootPin) Equal(other RootPin) bool { return p == other }

// PinRootContext observes this store's original rooted directory and requires
// that its canonical pathname still identifies the same private directory.
// The store retains its rooted handle; this method creates no state or locks.
func (s *Store) PinRootContext(ctx context.Context) (pin RootPin, resultErr error) {
	if s == nil || ctx == nil {
		return RootPin{}, os.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return RootPin{}, err
	}
	if s.closed {
		return RootPin{}, os.ErrClosed
	}
	directory, err := s.openDir(s.Root)
	if err != nil {
		return RootPin{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, directory.Close())
		if resultErr != nil {
			pin = RootPin{}
		}
	}()
	var pinned, visible unix.Stat_t
	if err = unix.Fstat(int(directory.Fd()), &pinned); err != nil {
		return RootPin{}, err
	}
	if err = unix.Lstat(s.Root, &visible); err != nil {
		return RootPin{}, fmt.Errorf("%w: observe store root: %w", task.ErrIdentityMismatch, err)
	}
	if privateRootIdentityMismatch(&pinned, &visible) {
		return RootPin{}, fmt.Errorf("%w: store root no longer identifies its pinned private directory", task.ErrIdentityMismatch)
	}
	if err = ctx.Err(); err != nil {
		return RootPin{}, err
	}
	return RootPin{root: s.Root, rootID: s.RootID, device: uint64(pinned.Dev), inode: uint64(pinned.Ino)}, nil
}

func privateRootIdentityMismatch(pinned, visible *unix.Stat_t) bool {
	return pinned.Ino == 0 || visible.Ino == 0 ||
		CheckPrivateStat(pinned, os.Geteuid(), true) != PrivateStatAccepted ||
		CheckPrivateStat(visible, os.Geteuid(), true) != PrivateStatAccepted ||
		pinned.Dev != visible.Dev || pinned.Ino != visible.Ino
}

// PinRootContext gives a passive reader the same identity observation without
// granting mutation authority or opening a maintenance lock.
func (r *ReadOnlyStore) PinRootContext(ctx context.Context) (RootPin, error) {
	if r == nil {
		return RootPin{}, os.ErrInvalid
	}
	return r.store.PinRootContext(ctx)
}
