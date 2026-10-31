package verifiedexec

// Legacy idle-image and abandoned-stage reclaim. See pool.go for the
// layout, owner and invariants.

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const maxLoggedFailureBytes = 240

func logPoolFailure(format string, args ...any) { log.Printf(format, args...) }

// reclaimResult counts what one reclaim pass deleted.
type reclaimResult struct{ Stages, Images, IdentityStages int }

// maintain runs best-effort idle-image reclaim after a release. It
// never waits for the gate, never returns an error and never touches a view: a
// failure is logged in bounded form and retried by the next release.
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
