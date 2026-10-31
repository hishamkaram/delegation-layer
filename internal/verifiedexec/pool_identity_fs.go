package verifiedexec

// Descriptor-relative I/O for the identity marker. Every operation below starts
// at one pinned descriptor of the pool root and opens each leaf with
// O_NOFOLLOW, so a leaf swapped for a symlink is refused rather than followed.
// This is a static check of one root: it is not a claim against a hostile
// concurrent replacement of an ancestor of the root.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

// pinnedRoot is a pool root held open by descriptor, with the pool whose fault
// seams its operations consult. The caller closes it.
type pinnedRoot struct {
	pool *Pool
	dir  *os.File
}

// pinRoot opens the pool root without following a symlink and requires the
// pinned directory itself to be private and owned by this user.
func (p *Pool) pinRoot() (*pinnedRoot, error) {
	fd, err := unix.Open(p.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("pin custody pool root %s: %w", p.root, err)
	}
	dir := os.NewFile(uintptr(fd), p.root)
	info, err := dir.Stat()
	if err == nil {
		err = requirePrivateInfo(info, p.root)
	}
	if err != nil {
		return nil, errors.Join(err, dir.Close())
	}
	return &pinnedRoot{pool: p, dir: dir}, nil
}

func (r *pinnedRoot) Close() error { return r.dir.Close() }

func (r *pinnedRoot) path(name string) string { return filepath.Join(r.pool.root, name) }

func (r *pinnedRoot) openLeaf(name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(r.dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, poolIdentityMode)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", r.path(name), err)
	}
	return os.NewFile(uintptr(fd), r.path(name)), nil
}

// openIdentity opens the marker for reading and requires a regular file owned
// by this user with exactly mode 0600 and a bounded size. O_NONBLOCK keeps a
// planted FIFO from blocking the open; the type check then refuses it. A link
// count above one is accepted: it is the twin a crash between the link and the
// removal of the stage leaves.
func (r *pinnedRoot) openIdentity() (*os.File, error) {
	file, err := r.openLeaf(poolIdentityName, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	if err = requireIdentityFile(file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func requireIdentityFile(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("stat %s: %w", file.Name(), err)
	}
	switch taskdir.CheckPrivateStat(&stat, os.Geteuid(), false) {
	case taskdir.PrivateStatAccepted:
	case taskdir.PrivateStatUnexpectedType:
		return fmt.Errorf("%w: %s is not a regular file", errPoolIdentityRecord, file.Name())
	case taskdir.PrivateStatUnsafeOwnerOrMode:
		return fmt.Errorf("%w: %s has an unsafe owner or mode", errPoolIdentityRecord, file.Name())
	default:
		return fmt.Errorf("%w: %s is not private", errPoolIdentityRecord, file.Name())
	}
	if stat.Size > maxPoolIdentityBytes {
		return fmt.Errorf("%w: %s is larger than %d bytes", errPoolIdentityRecord, file.Name(), maxPoolIdentityBytes)
	}
	return nil
}

// readIdentity is the strict read: the marker must exist and be the canonical
// record. A missing marker is reported as fs.ErrNotExist.
func (r *pinnedRoot) readIdentity() (string, error) {
	file, err := r.openIdentity()
	if err != nil {
		return "", err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxPoolIdentityBytes+1))
	if err = errors.Join(readErr, file.Close()); err != nil {
		return "", fmt.Errorf("read custody pool identity %s: %w", file.Name(), err)
	}
	if len(raw) > maxPoolIdentityBytes {
		return "", fmt.Errorf("%w: %s is larger than %d bytes", errPoolIdentityRecord, file.Name(), maxPoolIdentityBytes)
	}
	id, err := decodePoolIdentity(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", file.Name(), err)
	}
	return id, nil
}

// barrier flushes file with the platform's durability barrier, unless a test
// injected a failure for point in its place.
func (r *pinnedRoot) barrier(point poolPoint, file *os.File) error {
	if err := r.pool.injected(point); err != nil {
		return fmt.Errorf("flush %s: %w", file.Name(), err)
	}
	if err := taskdir.BarrierFile(file); err != nil {
		return fmt.Errorf("flush %s: %w", file.Name(), err)
	}
	return nil
}

// sweepStages removes every recognized marker stage and flushes the root when
// it removed any. Its caller holds the exclusive gate: no publisher that could
// still link a stage is alive, so a recognized stage is either this caller's own
// finished stage or an abandoned one. A name outside the recognized grammar is
// never touched, and a recognized name that is not a regular file of this user
// is reported and left alone.
func (r *pinnedRoot) sweepStages() (removed int, resultErr error) {
	if _, err := r.dir.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("rewind %s: %w", r.pool.root, err)
	}
	entries, err := r.dir.ReadDir(-1)
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", r.pool.root, err)
	}
	for _, entry := range entries {
		if !isIdentityStageName(entry.Name()) {
			continue
		}
		switch err = r.removeStage(entry.Name()); {
		case err == nil:
			removed++
		case errors.Is(err, fs.ErrNotExist):
		default:
			resultErr = errors.Join(resultErr, err)
		}
	}
	if removed == 0 {
		return 0, resultErr
	}
	return removed, errors.Join(resultErr, r.barrier(pointMarkerCleanBarrier, r.dir))
}

func (r *pinnedRoot) removeStage(name string) error {
	var stat unix.Stat_t
	fd := int(r.dir.Fd())
	if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat %s: %w", r.path(name), err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || int64(stat.Uid) != int64(os.Geteuid()) {
		return fmt.Errorf("%w: %s", errPoolIdentityStage, r.path(name))
	}
	if err := r.pool.injected(pointMarkerStageRemove); err != nil {
		return err
	}
	if err := unix.Unlinkat(fd, name, 0); err != nil {
		return fmt.Errorf("remove %s: %w", r.path(name), err)
	}
	return nil
}

// requireIdentity is the guard of every operation that reads or changes the pool
// on behalf of an owner. It proves, without creating or changing anything, that
// the recorded canonical private root holds the strictly canonical marker of the
// identity this pool is bound to. A missing root or marker, a mismatch, an
// alias, an unsafe owner or mode, a corrupt record and an unreadable root all
// leave the pool unresolved with the concrete cause: the caller must not report
// a release, reselect an identity or repair the root. An unbound pool is refused.
func (p *Pool) requireIdentity() error {
	if p.id == "" {
		return errPoolUnbound
	}
	if err := p.proveIdentity(); err != nil {
		return fmt.Errorf("%w: %w", errPoolIdentityUnverified, err)
	}
	p.at(pointIdentityChecked, "")
	return nil
}

func (p *Pool) proveIdentity() (resultErr error) {
	if err := p.requireCanonicalPrivateRoot(); err != nil {
		return err
	}
	root, err := p.pinRoot()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	id, err := root.readIdentity()
	if err != nil {
		return err
	}
	if id != p.id {
		return fmt.Errorf("%w: %s", errPoolIdentityMismatch, p.root)
	}
	return nil
}
