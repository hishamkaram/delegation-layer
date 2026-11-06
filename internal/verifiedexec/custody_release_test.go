package verifiedexec

// Release tests: the caller has already proven canonical closure, so these
// check only what ReleaseOwner and ReleasePending do with that fact.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestReleaseOwnerRemovesOnlyItsOwnViewsAndReclaimsIdleImages(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "shared", "0000000000000000000000000000000000000000000000000000000000000018")
	rig.seedView("a", entry, digest)
	rig.seedView("b", entry, digest)
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
	if pending, err := rig.pool.ReleasePending(rig.ns("a")); err != nil || pending {
		t.Fatalf("released owner still pending: %v %v", pending, err)
	}
	rig.requireViewsLinkImages("b", entry)
	if got := rig.images(); len(got) != 1 {
		t.Fatalf("a live owner's image was reclaimed: %v", got)
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("b")); err != nil {
		t.Fatal(err)
	}
	rig.requireImagesDir(0)
	entries, err := os.ReadDir(rig.pool.viewsPath())
	if err != nil || len(entries) != 0 {
		t.Fatalf("views directory after the last release: %v %v", entries, err)
	}
	rig.rec.note("release", "owner a released while b kept the shared image; the last release reclaimed it; no idle cache remains")
}

func TestReleaseOfAMissingNamespaceInALivePoolIsSuccess(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "other", "0000000000000000000000000000000000000000000000000000000000000013")
	rig.seedView("a", entry, digest)
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("ghost")); err != nil {
		t.Fatalf("release of a missing namespace in a live pool: %v", err)
	}
	rig.requireViewsLinkImages("a", entry)
}

// TestReleaseOfAMissingPoolIsUnresolvedNeverReleased: a guarded pool whose root
// is gone cannot prove that an owner's views are gone, so neither the release
// nor the pending check may answer "released".
func TestReleaseOfAMissingPoolIsUnresolvedNeverReleased(t *testing.T) {
	dir := scratch(t)
	root := filepath.Join(dir, "never-created")
	pool, err := NewPoolWithIdentity(root, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	ns := mustNamespace(t, "ghost")
	if err = pool.ReleaseOwner(context.Background(), ns); !errors.Is(err, errPoolIdentityUnverified) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("release of an owner in a missing pool gave %v; want an unverified identity caused by a missing root", err)
	}
	if pending, pendingErr := pool.ReleasePending(ns); !errors.Is(pendingErr, errPoolIdentityUnverified) || pending {
		t.Fatalf("pending %v %v for a missing pool; want an unverified identity", pending, pendingErr)
	}
	if _, err = os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a guarded read-only call created the pool: %v", err)
	}
}

func TestUnboundPoolRefusesEveryOwnerOperation(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "unbound", "000000000000000000000000000000000000000000000000000000000000001d")
	rig.seedView("a", entry, digest)
	unbound := &Pool{root: rig.pool.root, logf: logPoolFailure}
	before := treeSnapshot(t, rig.pool.root)
	ctx := context.Background()

	if err := unbound.ReleaseOwner(ctx, rig.ns("a")); !errors.Is(err, errPoolUnbound) {
		t.Fatalf("ReleaseOwner on an unbound pool gave %v; want %v", err, errPoolUnbound)
	}
	if pending, pendingErr := unbound.ReleasePending(rig.ns("a")); !errors.Is(pendingErr, errPoolUnbound) || pending {
		t.Fatalf("ReleasePending on an unbound pool gave %v %v; want %v", pending, pendingErr, errPoolUnbound)
	}
	if _, _, err := unbound.reclaimIdle(ctx); !errors.Is(err, errPoolUnbound) {
		t.Fatalf("reclaim on an unbound pool gave %v; want %v", err, errPoolUnbound)
	}
	if after := treeSnapshot(t, rig.pool.root); after != before {
		t.Fatalf("an unbound pool changed the root:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if unbound.Root() != rig.pool.root {
		t.Fatalf("Root() = %q; want %q", unbound.Root(), rig.pool.root)
	}
}

func TestReleaseNeverFollowsSymlinksOrChmodsOutsideTheOwner(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "symlink", "000000000000000000000000000000000000000000000000000000000000001c")
	command := rig.seedView("a", entry, digest)
	outside := filepath.Join(rig.dir, "installation")
	if err := os.MkdirAll(filepath.Join(outside, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	dependency := filepath.Join(outside, "dep.txt")
	if err := os.WriteFile(dependency, []byte("keep"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(outside, "nested"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(filepath.Join(outside, "nested"), 0o755); err != nil {
			t.Errorf("restore: %v", err)
		}
	})
	if err := unsealTemporaryDirectory(command.temporaryDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(command.temporaryDir, "installation")); err != nil {
		t.Fatal(err)
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]fs.FileMode{outside: 0o755, filepath.Join(outside, "nested"): 0o555, dependency: 0o640} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s after release: %v (%v), want mode %o", path, info, err, want)
		}
	}
}

func TestReleaseLeavesTheWitnessWhenAViewCannotBeRemoved(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "stuck", "000000000000000000000000000000000000000000000000000000000000001b")
	rig.seedView("a", entry, digest)
	owner := rig.ownerDir("a")
	if err := os.Chmod(owner, 0o500); err != nil {
		t.Fatal(err)
	}
	err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a"))
	if restoreErr := os.Chmod(owner, 0o700); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("release error %v; want the concrete permission cause", err)
	}
	if pending, pendingErr := rig.pool.ReleasePending(rig.ns("a")); pendingErr != nil || !pending {
		t.Fatalf("the witness did not survive a partial release: %v %v", pending, pendingErr)
	}
	if got := rig.viewDirs("a"); len(got) != 1 {
		t.Fatalf("views after the failed removal: %v", got)
	}
	if err = rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatalf("the retry after the fault was repaired: %v", err)
	}
	if rig.pending("a") {
		t.Fatal("the witness outlived a complete release")
	}
}

func TestReleaseRefusesAWitnessThatIsNotADirectory(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "file-witness", "000000000000000000000000000000000000000000000000000000000000000b")
	rig.seedView("seed", entry, digest)
	witness := rig.ownerDir("a")
	if err := os.WriteFile(witness, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); !errors.Is(err, errPoolRootNotPrivate) {
		t.Fatalf("release gave %v; want %v", err, errPoolRootNotPrivate)
	}
	if content, err := os.ReadFile(witness); err != nil || string(content) != "not ours" {
		t.Fatalf("the stranger file was touched: %q %v", content, err)
	}
}

func TestReleasePendingReportsConcreteErrors(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "pending", "0000000000000000000000000000000000000000000000000000000000000016")
	rig.seedView("a", entry, digest)
	if pending, err := rig.pool.ReleasePending(rig.ns("a")); err != nil || !pending {
		t.Fatalf("bound owner: %v %v", pending, err)
	}
	if pending, err := rig.pool.ReleasePending(rig.ns("other")); err != nil || pending {
		t.Fatalf("unknown owner: %v %v", pending, err)
	}
	views := rig.pool.viewsPath()
	if err := os.Chmod(views, 0); err != nil {
		t.Fatal(err)
	}
	_, err := rig.pool.ReleasePending(rig.ns("a"))
	if restoreErr := os.Chmod(views, 0o700); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("an unreadable views directory gave %v; want the concrete permission cause", err)
	}
}

func TestReleaseRemovesTheWitnessLast(t *testing.T) {
	rig := newPoolRig(t)
	first, firstDigest := plainEntry(t, rig.dir, "last-1", "0000000000000000000000000000000000000000000000000000000000000010")
	second, secondDigest := plainEntry(t, rig.dir, "last-2", "0000000000000000000000000000000000000000000000000000000000000011")
	rig.seedView("a", first, firstDigest)
	rig.seedView("a", second, secondDigest)
	removed, beforeWitness := 0, 0
	rig.pool.faults.at = func(point poolPoint, _ string) {
		if point == pointIdentityChecked {
			// The identity is proved again by the reclaim that follows the
			// release, after the witness is legitimately gone; this test
			// judges the removal points only.
			return
		}
		_, witnessErr := os.Lstat(rig.ownerDir("a"))
		if witnessErr != nil {
			t.Errorf("witness gone at %s: %v", point, witnessErr)
		}
		if point == pointAfterViewRemoved {
			removed++
		}
		if point == pointBeforeWitnessRemove {
			beforeWitness++
			if got := rig.viewDirs("a"); len(got) != 0 {
				t.Errorf("views %v remained when only the witness was left to remove", got)
			}
		}
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
	if removed != 2 || beforeWitness != 1 {
		t.Fatalf("release points fired %d/%d times; want 2 view removals then 1 witness removal", removed, beforeWitness)
	}
	if rig.pending("a") {
		t.Fatal("witness survived")
	}
}

func TestReleaseHonorsContextAndKeepsTheWitness(t *testing.T) {
	rig := newPoolRig(t)
	first, firstDigest := plainEntry(t, rig.dir, "ctx-1", "0000000000000000000000000000000000000000000000000000000000000009")
	second, secondDigest := plainEntry(t, rig.dir, "ctx-2", "000000000000000000000000000000000000000000000000000000000000000a")
	rig.seedView("a", first, firstDigest)
	rig.seedView("a", second, secondDigest)
	ctx, cancel := context.WithCancel(context.Background())
	rig.pool.faults.at = func(point poolPoint, _ string) {
		if point == pointAfterViewRemoved {
			cancel()
		}
	}
	err := rig.pool.ReleaseOwner(ctx, rig.ns("a"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("release gave %v; want context.Canceled", err)
	}
	if pending, pendingErr := rig.pool.ReleasePending(rig.ns("a")); pendingErr != nil || !pending {
		t.Fatalf("an interrupted release dropped the witness: %v %v", pending, pendingErr)
	}
	if got := rig.viewDirs("a"); len(got) != 1 {
		t.Fatalf("views after the interrupted release: %v", got)
	}
	rig.pool.faults.at = nil
	if err = rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
}

func TestIdleReclaimFailureNeverChangesTheReleaseOutcomeAndRetriesLater(t *testing.T) {
	for _, retry := range []string{"release"} {
		t.Run(retry, func(t *testing.T) {
			rig := newPoolRig(t)
			entry, digest := plainEntry(t, rig.dir, "gc-failure", "000000000000000000000000000000000000000000000000000000000000000c")
			rig.seedView("a", entry, digest)
			releaseWithBrokenReclaim(t, rig, "a")
			if !rig.imageExists(digest) {
				t.Fatal("the failed reclaim removed the image")
			}
			if !logContains(rig.logged(), "idle image reclaim failed") {
				t.Fatalf("the failure was not reported: %q", rig.logged())
			}
			if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("ghost")); err != nil {
				t.Fatal(err)
			}
			if rig.imageExists(digest) {
				t.Fatalf("the later %s did not retry the reclaim", retry)
			}
		})
	}
}

// releaseWithBrokenReclaim releases owner while the images directory refuses
// deletions, and requires the release itself to succeed and drop the witness.
func releaseWithBrokenReclaim(t *testing.T, rig *poolRig, owner string) {
	t.Helper()
	images := rig.pool.imagesPath()
	if err := os.Chmod(images, 0o500); err != nil {
		t.Fatal(err)
	}
	err := rig.pool.ReleaseOwner(context.Background(), rig.ns(owner))
	if restoreErr := os.Chmod(images, 0o700); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err != nil {
		t.Fatalf("a reclaim fault changed the release outcome: %v", err)
	}
	if rig.pending(owner) {
		t.Fatal("a reclaim fault kept the released owner's witness")
	}
}

func logContains(lines []string, want string) bool {
	for _, line := range lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// busyGateDeadline bounds how long a release may take while another holder owns
// the gate exclusively. The real release returns in milliseconds.
const busyGateDeadline = 5 * time.Second

// releaseBehindHeldGate runs ReleaseOwner in a goroutine while holder keeps the
// gate. It always unlocks and closes the holder and always joins the goroutine,
// so a release that wrongly waits for the gate is reported as blocked (not as a
// package timeout) and nothing outlives the test.
func releaseBehindHeldGate(t *testing.T, rig *poolRig, holder *taskdir.LockFile, owner string) (waited time.Duration, blocked bool, releaseErr error) {
	t.Helper()
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- rig.pool.ReleaseOwner(context.Background(), rig.ns(owner)) }()
	select {
	case releaseErr = <-done:
		waited = time.Since(started)
	case <-time.After(busyGateDeadline):
		blocked = true
	}
	if unlockErr := errors.Join(holder.Unlock(), holder.Close()); unlockErr != nil {
		t.Errorf("release the held gate: %v", unlockErr)
	}
	if blocked {
		releaseErr = <-done
		waited = time.Since(started)
	}
	return waited, blocked, releaseErr
}

func TestIdleReclaimBusyGateReturnsImmediatelyAndLosesNoLiveOwner(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "busy", "0000000000000000000000000000000000000000000000000000000000000001")
	live, liveDigest := plainEntry(t, rig.dir, "busy-live", "0000000000000000000000000000000000000000000000000000000000000002")
	rig.seedView("a", entry, digest)
	rig.seedView("live", live, liveDigest)
	holder, err := taskdir.OpenLockFileContext(context.Background(), rig.pool.gatePath(), taskdir.LockLevelNone)
	if err != nil {
		t.Fatal(err)
	}
	if err = holder.LockEXNonblocking(); err != nil {
		t.Fatal(err)
	}
	waited, blocked, releaseErr := releaseBehindHeldGate(t, rig, holder, "a")
	if blocked {
		t.Fatalf("release blocked for %s behind a held gate; reclaim must skip a busy gate, not wait for it", busyGateDeadline)
	}
	if releaseErr != nil {
		t.Fatalf("release behind a busy gate gave %v after %s", releaseErr, waited)
	}
	if rig.pending("a") {
		t.Fatal("a busy gate kept the released owner's witness")
	}
	if !logContains(rig.logged(), "gate busy") {
		t.Fatalf("the skipped reclaim was not reported: %q", rig.logged())
	}
	rig.requireViewsLinkImages("live", live)
	if got := rig.images(); len(got) != 2 {
		t.Fatalf("images after the skipped reclaim: %v; want both retained", got)
	}
	if err = rig.pool.ReleaseOwner(context.Background(), rig.ns("ghost")); err != nil {
		t.Fatal(err)
	}
	if got := rig.images(); len(got) != 1 || got[0] != liveDigest {
		t.Fatalf("images after the retry: %v; want only the live owner's", got)
	}
	rig.rec.note("gate-busy", "release returned in %s; the skipped reclaim ran on the next release", waited.Round(time.Millisecond))
}

func TestIdleReclaimRemovesStagesAndOnlyUnlinkedImages(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "stages", "000000000000000000000000000000000000000000000000000000000000001a")
	rig.seedView("keep", entry, digest)
	stage := filepath.Join(rig.pool.imagesPath(), poolStagePrefix+"abandoned")
	stranger := filepath.Join(rig.pool.imagesPath(), "not-ours")
	idle := filepath.Join(rig.pool.imagesPath(), strings.Repeat("a", 64))
	for _, path := range []string{stage, stranger, idle} {
		if err := os.WriteFile(path, []byte("x"), poolImageMode); err != nil {
			t.Fatal(err)
		}
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("ghost")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{stage: false, idle: false, stranger: true, rig.pool.imagePath(digest): true} {
		_, err := os.Lstat(path)
		if exists := err == nil; exists != want {
			t.Fatalf("%s exists=%v, want %v (%v)", filepath.Base(path), exists, want, err)
		}
	}
	rig.requireViewsLinkImages("keep", entry)
}
