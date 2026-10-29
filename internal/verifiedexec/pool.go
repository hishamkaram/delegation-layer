package verifiedexec

// Pool is the filesystem custody of verified executables. It owns images and
// views and nothing else.
//
// Layout below one private root, all on one filesystem:
//
//	<root>/.gate                    taskdir lock: shared to link, exclusive to publish and reclaim
//	<root>/.identity                immutable pool identity marker, mode 0600, published once
//	<root>/.identity-stage-<hex32>  at most one live marker stage, owned by the exclusive gate holder
//	<root>/images/<sha256>          immutable, mode 0500, one per digest
//	<root>/images/.custody-stage-*  at most one live stage, owned by the exclusive gate holder
//	<root>/views/<namespace>/       an owner's witness directory
//	<root>/views/<namespace>/delegation-layer-verified-*   one private hard-link view per bind
//
// Owner: Pool. Writers: Prepare (the identity marker, once, and the layout),
// Bind (views, and images through the gate), ReleaseOwner (this owner's views
// and witness) and idle reclaim (marker stages, image stages and images no view
// links). Readers: the bound child, through its view.
// Invariants: an image holds exactly the bytes its name claims and is never
// rewritten; a view entry is a hard link to a verified image, never to a source
// inode; a stage or an image with link count 1 observed under the exclusive gate
// belongs to a dead publisher or an unreferenced digest, because a live
// publisher holds the gate from stage creation until its first view link exists.
// The identity marker is published once and never changes; every operation on a
// pool bound to an identity first proves that the canonical private root still
// holds that exact marker, and otherwise changes nothing and reports why.
//
// Pool never decides that an owner is closed. Its caller proves canonical
// closure and then calls ReleaseOwner. A dead process, an unheld lock or an
// inherited descriptor is never evidence here.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

const (
	poolImagesDir   = "images"
	poolViewsDir    = "views"
	poolGateName    = ".gate"
	poolStagePrefix = ".custody-stage-"
	poolDirMode     = 0o700
	poolImageMode   = 0o500
	poolViewPrefix  = "delegation-layer-verified-"
)

var (
	errPoolRootInvalid    = errors.New("custody pool root must be an absolute clean path")
	errPoolRootNotPrivate = errors.New("custody pool directory is not a private directory owned by this user")
	errPoolRootNotCanon   = errors.New("custody pool root is not canonical (a symlink or alias is in its path)")
	errPoolCrossDevice    = errors.New("custody pool images and views are on different filesystems")
)

// ErrPoolNoExec is the cause of a refusal to use a pool root on a filesystem
// mounted noexec. It is exported so that a caller can classify the clean refusal
// with the canonical error-cause walker.
var ErrPoolNoExec = errors.New("custody pool filesystem is mounted noexec; views could not be executed")

// Pool is safe for concurrent use. It holds only its root, the identity it is
// bound to and test seams; every operation opens its own gate handle because a
// taskdir lock handle is not safe for concurrent binders. id is empty for an
// unbound pool (NewPool), which may only Prepare; a pool made by
// NewPoolWithIdentity holds a valid id for its whole life and never Prepares.
type Pool struct {
	root   string
	id     string
	logf   func(format string, args ...any)
	faults poolFaults
}

// poolPoint names a place where a test may inject a fault or a crash.
type poolPoint string

const (
	pointBeforeExclusive poolPoint = "before-exclusive"
	pointBeforeCopy      poolPoint = "before-copy"
	pointAfterStage      poolPoint = "after-stage"
	pointAfterImageLink  poolPoint = "after-image-link"
	pointAfterViewLink   poolPoint = "after-view-link"
	// pointSourceOpened fires in Bind right after the original executable has
	// been opened and verified, before the builder reads anything from it. It
	// names the window in which an installer can still rewrite the original.
	pointSourceOpened poolPoint = "source-opened"
	// pointStageWritten fires after a stage has been written, made read-only and
	// flushed, immediately before it is re-opened and verified by digest.
	pointStageWritten poolPoint = "stage-written"
	// Release points carry no digest: the owner namespace is the identity.
	pointAfterViewRemoved    poolPoint = "after-view-removed"
	pointBeforeWitnessRemove poolPoint = "before-witness-remove"
	// pointIdentityChecked fires each time a guarded operation has just proved
	// the identity marker, before it acts on that proof.
	pointIdentityChecked poolPoint = "identity-checked"
	// pointMarkerLinked fires right after the marker was linked into place,
	// before its directory is flushed.
	pointMarkerLinked poolPoint = "marker-linked"
)

// Marker points are consulted through poolFaults.inject, immediately before the
// production operation they name, which an injected error replaces.
const (
	pointMarkerStageBarrier poolPoint = "marker-stage-barrier"
	pointMarkerLink         poolPoint = "marker-link"
	pointMarkerFileBarrier  poolPoint = "marker-file-barrier"
	pointMarkerDirBarrier   poolPoint = "marker-dir-barrier"
	pointMarkerStageRemove  poolPoint = "marker-stage-remove"
	pointMarkerCleanBarrier poolPoint = "marker-clean-barrier"
)

// poolFaults is a test seam; the zero value does nothing.
type poolFaults struct {
	link   func(oldname, newname string) error
	at     func(point poolPoint, digest string)
	inject func(point poolPoint) error
}

// NewPool names an unbound custody pool rooted at root, which must be absolute
// and clean. It touches nothing, and the pool it returns may only Prepare and
// report its Root: Bind, ReleaseOwner and ReleasePending refuse it with
// errPoolUnbound until a caller binds an identity with NewPoolWithIdentity.
func NewPool(root string) (*Pool, error) {
	if err := validatePoolRoot(root); err != nil {
		return nil, err
	}
	return &Pool{root: root, logf: logPoolFailure}, nil
}

// NewPoolWithIdentity names the custody pool rooted at root that is expected to
// hold the identity id, which Prepare returned. id is an immutable guard, not a
// second authority: every operation proves that the canonical private root
// still holds exactly that marker before it changes anything. An empty or
// malformed id is refused here and never yields an unguarded pool. It touches
// nothing.
func NewPoolWithIdentity(root, id string) (*Pool, error) {
	if err := validatePoolRoot(root); err != nil {
		return nil, err
	}
	if err := task.ValidateSHA256(id); err != nil {
		return nil, fmt.Errorf("%w: %w", errPoolIdentityInvalid, err)
	}
	return &Pool{root: root, id: id, logf: logPoolFailure}, nil
}

func validatePoolRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("%w: %q", errPoolRootInvalid, root)
	}
	return nil
}

// Root is the pool's root directory.
func (p *Pool) Root() string { return p.root }

func (p *Pool) gatePath() string   { return filepath.Join(p.root, poolGateName) }
func (p *Pool) imagesPath() string { return filepath.Join(p.root, poolImagesDir) }
func (p *Pool) viewsPath() string  { return filepath.Join(p.root, poolViewsDir) }
func (p *Pool) imagePath(digest string) string {
	return filepath.Join(p.imagesPath(), digest)
}

func (p *Pool) at(point poolPoint, digest string) {
	if p.faults.at != nil {
		p.faults.at(point, digest)
	}
}

// injected returns the error a test injected in place of the production
// operation named by point, or nil.
func (p *Pool) injected(point poolPoint) error {
	if p.faults.inject != nil {
		return p.faults.inject(point)
	}
	return nil
}

func (p *Pool) linkFile(oldname, newname string) error {
	if p.faults.link != nil {
		return p.faults.link(oldname, newname)
	}
	return os.Link(oldname, newname)
}

// openGate prepares and verifies the layout, then opens this operation's own
// gate handle with the canonical context-aware taskdir lock opener. The layout
// is verified before any stage, image or view is published.
func (p *Pool) openGate(ctx context.Context) (*taskdir.LockFile, error) {
	if info, err := os.Lstat(p.root); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("%w: %s", errPoolRootNotPrivate, p.root)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	gate, err := taskdir.OpenLockFileContext(ctx, p.gatePath(), taskdir.LockLevelNone)
	if err != nil {
		return nil, err
	}
	if err = p.verifyLayout(); err != nil {
		return nil, errors.Join(err, gate.Close())
	}
	return gate, nil
}

// verifyLayout checks the root and creates the two subtrees, refusing aliases,
// foreign ownership, group or world access, a noexec mount and a device split.
func (p *Pool) verifyLayout() error {
	if err := p.requireCanonicalPrivateRoot(); err != nil {
		return err
	}
	noExec, err := filesystemForbidsExec(p.root)
	if err != nil {
		return err
	}
	if noExec {
		return fmt.Errorf("%w: %s", ErrPoolNoExec, p.root)
	}
	for _, dir := range []string{p.imagesPath(), p.viewsPath()} {
		if err = ensurePrivateDirectory(dir, p.root); err != nil {
			return err
		}
	}
	return p.requireOneDevice()
}

// requireCanonicalPrivateRoot is the one read-only check of the pool root that
// bind, release, pending and reclaim all share: no alias in its path, and a
// private directory owned by this user.
func (p *Pool) requireCanonicalPrivateRoot() error {
	canonical, err := config.CanonicalizePath(p.root)
	if err != nil {
		return err
	}
	if canonical != p.root {
		return fmt.Errorf("%w: %s resolves to %s", errPoolRootNotCanon, p.root, canonical)
	}
	return requirePrivateDirectory(p.root)
}

// observeRoot validates the pool root read-only. A root that exists must be
// canonical and private. A root that does not exist is absent only when the
// ancestry that does exist is canonical too: a missing pool behind a live or
// dangling symlinked ancestor is a refusal, not "nothing to do".
func (p *Pool) observeRoot() (present bool, err error) {
	_, err = os.Lstat(p.root)
	if err == nil {
		return true, p.requireCanonicalPrivateRoot()
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return false, p.requireCanonicalAncestry()
}

// requireCanonicalAncestry checks the existing part of the ancestry of a root
// that is not there yet. The canonical resolver follows live symlinks; it cannot
// see a dangling one among the missing components, so the nearest existing
// ancestor must also be a real directory.
func (p *Pool) requireCanonicalAncestry() error {
	canonical, err := config.CanonicalizePath(p.root)
	if err != nil {
		return err
	}
	if canonical != p.root {
		return fmt.Errorf("%w: %s resolves to %s", errPoolRootNotCanon, p.root, canonical)
	}
	for dir := filepath.Dir(p.root); ; dir = filepath.Dir(dir) {
		info, statErr := os.Lstat(dir)
		if statErr == nil {
			if !info.IsDir() {
				return fmt.Errorf("%w: ancestor %s is not a directory", errPoolRootNotCanon, dir)
			}
			return nil
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return statErr
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

// observeDirectory validates the canonical private chain root -> names...
// without creating or changing anything, so a caller may observe or remove
// entries below it. present is false only for a genuine ENOENT at some link of
// the chain. A symlink at any link, dangling or not, is a refusal, never absent.
// It is a static check: it does not defend against a hostile concurrent
// replacement of an ancestor.
func (p *Pool) observeDirectory(names ...string) (present bool, err error) {
	if present, err = p.observeRoot(); err != nil || !present {
		return false, err
	}
	dir := p.root
	for _, name := range names {
		dir = filepath.Join(dir, name)
		if err = requirePrivateDirectory(dir); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

func (p *Pool) requireOneDevice() error {
	var root uint64
	for i, dir := range []string{p.root, p.imagesPath(), p.viewsPath()} {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("custody pool directory has no device identity")
		}
		device := uint64(stat.Dev)
		if i == 0 {
			root = device
		} else if device != root {
			return errors.Join(fmt.Errorf("%w: %s", errPoolCrossDevice, dir), unix.EXDEV)
		}
	}
	return nil
}

// requirePrivateDirectory refuses anything but a real directory owned by this
// user with no group or world access.
func requirePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return requirePrivateInfo(info, path)
}

// requirePrivateInfo is the one private-directory decision, over an Lstat of a
// path or an Fstat of a pinned descriptor.
func requirePrivateInfo(info os.FileInfo, path string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || uint64(stat.Uid) != uint64(os.Geteuid()) {
		return fmt.Errorf("%w: %s", errPoolRootNotPrivate, path)
	}
	return nil
}

// ensurePrivateDirectory creates dir if missing and then requires it to be
// private. The parent is flushed whether this call created dir or found it: an
// earlier attempt may have created it and then failed its barrier, so an
// existing entry is not yet known to be durable.
func ensurePrivateDirectory(dir, parent string) error {
	if err := os.Mkdir(dir, poolDirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := barrierPath(parent); err != nil {
		return err
	}
	return requirePrivateDirectory(dir)
}

// barrierPath flushes a file or directory with the platform's required
// durability barrier, the canonical pattern for this repository.
func barrierPath(path string) (resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	return taskdir.BarrierFile(file)
}

func linkCount(info os.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("file has no link count")
	}
	return widen(stat.Nlink), nil
}

// widen exists because Stat_t.Nlink is uint16 on Darwin and uint64 on Linux.
func widen[T ~uint16 | ~uint32 | ~uint64](value T) uint64 { return uint64(value) }

func isDigestName(name string) bool { return isLowerHex(name, 64) }

// isLowerHex reports whether text is exactly length lowercase hexadecimal digits.
func isLowerHex(text string, length int) bool {
	if len(text) != length {
		return false
	}
	for _, r := range text {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
