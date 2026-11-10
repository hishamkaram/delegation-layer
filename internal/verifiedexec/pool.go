package verifiedexec

// Pool disposes legacy executable custody after the guardian proves closure.
// The guardian's saved registration owns the exact root, identity and namespace.
// Pool never infers closure or creates pools, images or views.
//
// Legacy layout: .identity (canonical JSON, 0600), .gate, images/<sha256>
// (0500), and views/<namespace>/delegation-layer-verified-* (0500).
// The root, images, views and namespace directories are private (0700).
// Every operation proves the immutable identity and canonical private chain.
// Release removes only its owner's views, then its witness. Reclaim uses the
// exclusive gate to remove abandoned stages and images with no view links.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	poolImagesDir   = "images"
	poolViewsDir    = "views"
	poolGateName    = ".gate"
	poolStagePrefix = ".custody-stage-"
	poolViewPrefix  = "delegation-layer-verified-"
)

var (
	errPoolRootInvalid    = errors.New("custody pool root must be an absolute clean path")
	errPoolRootNotPrivate = errors.New("custody pool directory is not a private directory owned by this user")
	errPoolRootNotCanon   = errors.New("custody pool root is not canonical (a symlink or alias is in its path)")
)

// Pool is safe for concurrent disposal. Each operation opens its own gate.
// The recorded identity is immutable; zero-value pools refuse every operation.
type Pool struct {
	root   string
	id     string
	logf   func(format string, args ...any)
	faults poolFaults
}

// poolPoint names a place where a test may inject a fault or a crash.
type poolPoint string

const (
	// Release points carry no digest: the owner namespace is the identity.
	pointAfterViewRemoved    poolPoint = "after-view-removed"
	pointBeforeWitnessRemove poolPoint = "before-witness-remove"
	// pointIdentityChecked fires each time a guarded operation has just proved
	// the identity marker, before it acts on that proof.
	pointIdentityChecked poolPoint = "identity-checked"
)

// Marker points are consulted through poolFaults.inject, immediately before the
// production operation they name, which an injected error replaces.
const (
	pointMarkerStageRemove  poolPoint = "marker-stage-remove"
	pointMarkerCleanBarrier poolPoint = "marker-clean-barrier"
)

// poolFaults is a test seam; the zero value does nothing.
type poolFaults struct {
	at     func(point poolPoint, digest string)
	inject func(point poolPoint) error
}

// NewPoolWithIdentity names the custody pool rooted at root that is expected to
// hold the recorded legacy identity id. id is an immutable guard, not a
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

// requireCanonicalPrivateRoot is the one read-only check of the pool root that
// release, pending and reclaim all share: no alias in its path, and a
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

// WithFailureReporter returns an immutable invocation-specific view. It changes
// only best-effort telemetry; recorded custody remains the removal authority.
func (p *Pool) WithFailureReporter(report func(error)) *Pool {
	if report == nil {
		return p
	}
	copy := *p
	copy.logf = func(format string, args ...any) { report(fmt.Errorf(format, args...)) }
	return &copy
}
