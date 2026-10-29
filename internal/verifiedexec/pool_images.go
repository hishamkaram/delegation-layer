package verifiedexec

// Image publication, linking and idle reclaim for Pool. See pool.go for the
// layout, owner and invariants.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

var errPoolImageConflict = errors.New("custody image path holds different bytes than its name")

const maxLoggedFailureBytes = 240

func logPoolFailure(format string, args ...any) { log.Printf(format, args...) }

// poolBinder is one Bind's view of the pool: its own gate handle and context.
type poolBinder struct {
	pool *Pool
	gate *taskdir.LockFile
	ctx  context.Context
}

// materialize is the fileMaterializer of a pooled view: it links the verified
// image of digest into destination, publishing the image first when none
// exists. Reading the image back is part of every use; an image is trusted only
// after its bytes hash to its name.
func (b *poolBinder) materialize(source *os.File, destination, digest string) error {
	if !isDigestName(digest) {
		return fmt.Errorf("custody image name %q is not a sha256 digest", digest)
	}
	linked, err := b.linkShared(destination, digest)
	if err != nil || linked {
		return err
	}
	return b.populate(source, destination, digest)
}

func (b *poolBinder) lock(exclusive bool) error {
	if exclusive {
		return b.gate.LockEX(b.ctx)
	}
	return b.gate.LockSH(b.ctx)
}

func (b *poolBinder) linkShared(destination, digest string) (linked bool, resultErr error) {
	if err := b.lock(false); err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, b.gate.Unlock()) }()
	return b.linkExisting(destination, digest)
}

// linkExisting reports false when no image is published for digest.
func (b *poolBinder) linkExisting(destination, digest string) (bool, error) {
	image := b.pool.imagePath(digest)
	if _, err := os.Lstat(image); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, b.linkVerified(image, destination, digest)
}

// linkVerified hashes the image, links it, and checks that the new name is the
// very inode that was hashed.
func (b *poolBinder) linkVerified(image, destination, digest string) (resultErr error) {
	file, err := Open(image, digest)
	if err != nil {
		return fmt.Errorf("verify custody image: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if err = b.pool.linkFile(image, destination); err != nil {
		return linkFailure(err)
	}
	verified, statErr := file.Stat()
	named, lstatErr := os.Lstat(destination)
	if err = errors.Join(statErr, lstatErr); err != nil {
		return err
	}
	if !os.SameFile(verified, named) {
		return errors.New("custody view entry is not the verified image")
	}
	b.pool.at(pointAfterViewLink, digest)
	return nil
}

// linkFailure never lets a cross-filesystem link fall back to a copy.
func linkFailure(err error) error {
	if errors.Is(err, unix.EXDEV) {
		return errors.Join(errPoolCrossDevice, err)
	}
	return fmt.Errorf("link custody image: %w", err)
}

// populate publishes a missing image and links its first view while holding the
// exclusive gate, so reclaim can never see the new image without a view.
func (b *poolBinder) populate(source *os.File, destination, digest string) (resultErr error) {
	b.pool.at(pointBeforeExclusive, digest)
	if err := b.lock(true); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, b.gate.Unlock()) }()
	if linked, err := b.linkExisting(destination, digest); err != nil || linked {
		return err
	}
	b.pool.at(pointBeforeCopy, digest)
	stage, err := b.pool.stage(source, digest)
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(stage); err != nil && !errors.Is(err, fs.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	b.pool.at(pointAfterStage, digest)
	return b.publish(stage, destination, digest)
}

// publish links the stage to the image name without replacing anything. An
// existing image is the winner of a create-once race and must verify.
func (b *poolBinder) publish(stage, destination, digest string) error {
	image := b.pool.imagePath(digest)
	existed := false
	if err := b.pool.linkFile(stage, image); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return linkFailure(err)
		}
		existed = true
	}
	b.pool.at(pointAfterImageLink, digest)
	if err := barrierPath(b.pool.imagesPath()); err != nil {
		return err
	}
	if err := b.linkVerified(image, destination, digest); err != nil {
		if existed {
			return errors.Join(errPoolImageConflict, err)
		}
		return err
	}
	return nil
}

// stage copies the already-verified source while hashing the bytes actually
// written, closes the writable descriptor, then makes the file read-only,
// durable and re-verified by name.
func (p *Pool) stage(source *os.File, digest string) (string, error) {
	target, err := os.CreateTemp(p.imagesPath(), poolStagePrefix+"*")
	if err != nil {
		return "", err
	}
	path := target.Name()
	if err = p.fillStage(target, path, source, digest); err != nil {
		return "", errors.Join(err, os.Remove(path))
	}
	return path, nil
}

func (p *Pool) fillStage(target *os.File, path string, source *os.File, digest string) error {
	if err := copyVerifiedFile(target, source, digest); err != nil {
		return err
	}
	if err := os.Chmod(path, poolImageMode); err != nil {
		return err
	}
	if err := barrierPath(path); err != nil {
		return err
	}
	p.at(pointStageWritten, digest)
	verified, err := Open(path, digest)
	if err != nil {
		return err
	}
	return verified.Close()
}

// reclaimResult counts what one reclaim pass deleted.
type reclaimResult struct{ Stages, Images, IdentityStages int }

// maintain runs best-effort idle-image reclaim after a Bind or a release. It
// never waits for the gate, never returns an error and never touches a view: a
// failure is logged in bounded form and retried by the next release or bind.
func (p *Pool) maintain(ctx context.Context, reportBusy bool) {
	if ctx.Err() != nil {
		return
	}
	_, busy, err := p.reclaimIdle(ctx)
	switch {
	case err != nil:
		p.logf("custody pool %s: idle image reclaim failed: %s", p.root, bounded(err.Error()))
	case busy && reportBusy:
		p.logf("custody pool %s: idle image reclaim skipped: gate busy", p.root)
	}
}

func bounded(text string) string {
	if len(text) > maxLoggedFailureBytes {
		return text[:maxLoggedFailureBytes] + "..."
	}
	return text
}

// reclaimIdle deletes abandoned stages first, then images no view links (link
// count 1). It takes the exclusive gate without waiting; busy means skipped. It
// creates nothing: a pool with no gate has nothing to do.
//
// The identity is proved before the gate is opened and again right after it is
// acquired, so a root replaced meanwhile loses nothing to a destructive pass.
func (p *Pool) reclaimIdle(ctx context.Context) (result reclaimResult, busy bool, resultErr error) {
	if err := p.requireIdentity(); err != nil {
		return result, false, err
	}
	// The images chain is validated before the gate is opened or anything is
	// removed, so reclaim never follows an alias out of the pool.
	if present, err := p.observeDirectory(poolImagesDir); err != nil || !present {
		return result, false, err
	}
	if _, err := os.Lstat(p.gatePath()); errors.Is(err, fs.ErrNotExist) {
		return result, false, nil
	} else if err != nil {
		return result, false, err
	}
	gate, err := taskdir.OpenLockFileContext(ctx, p.gatePath(), taskdir.LockLevelNone)
	if err != nil {
		return result, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, gate.Close()) }()
	if err = gate.LockEXNonblocking(); err != nil {
		if errors.Is(err, task.ErrLockBusy) {
			return result, true, nil
		}
		return result, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, gate.Unlock()) }()
	if err = p.requireIdentity(); err != nil {
		return result, false, err
	}
	result, err = p.removeIdle()
	return result, false, err
}

func (p *Pool) removeIdle() (reclaimResult, error) {
	var result reclaimResult
	stages, err := p.removeMatching(func(name string) bool { return strings.HasPrefix(name, poolStagePrefix) }, nil)
	result.Stages = stages
	if err != nil {
		return result, err
	}
	images, err := p.removeMatching(isDigestName, func(info os.FileInfo) (bool, error) {
		count, countErr := linkCount(info)
		return count == 1, countErr
	})
	result.Images = images
	if err != nil {
		return result, err
	}
	if result.Stages+result.Images > 0 {
		if err = barrierPath(p.imagesPath()); err != nil {
			return result, err
		}
	}
	result.IdentityStages, err = p.sweepIdentityStages()
	return result, err
}

// sweepIdentityStages removes the abandoned marker stages of the root; its
// caller holds the exclusive gate.
func (p *Pool) sweepIdentityStages() (removed int, resultErr error) {
	root, err := p.pinRoot()
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	return root.sweepStages()
}

func (p *Pool) removeMatching(match func(string) bool, eligible func(os.FileInfo) (bool, error)) (int, error) {
	entries, err := os.ReadDir(p.imagesPath())
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if !match(entry.Name()) {
			continue
		}
		path := filepath.Join(p.imagesPath(), entry.Name())
		if eligible != nil {
			info, statErr := os.Lstat(path)
			if statErr != nil {
				return removed, statErr
			}
			ok, eligibleErr := eligible(info)
			if eligibleErr != nil {
				return removed, eligibleErr
			}
			if !ok {
				continue
			}
		}
		if err = os.Remove(path); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
