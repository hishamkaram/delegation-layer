package verifiedexec

// Interrupted creation states are literal legacy fixtures; no producer remains.
// Release crashes use a real child exiting at the named removal point, followed
// by the parent's exact Wait. Only closure-authorized ReleaseOwner removes views.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stageNames(rig *poolRig) []string {
	var stages []string
	for _, name := range rig.imagesDirEntries() {
		if strings.HasPrefix(name, poolStagePrefix) {
			stages = append(stages, name)
		}
	}
	return stages
}

func TestInterruptedCopyLeavesTheWitnessAndAnEmptyView(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "crash-before-copy", "0000000000000000000000000000000000000000000000000000000000000004")
	seedEmptyLegacyView(t, rig, "a")
	rig.requireImagesDir(0)
	// The view directory is made before the first file is materialized; the
	// crash leaves it empty and closure-owned.
	rig.requireViewDirs("a", 1)
	if files, err := viewEntries(rig.ownerDir("a")); err != nil || len(files) != 0 {
		t.Fatalf("the crashed view holds files %v (%v)", files, err)
	}
	if pending, err := rig.pool.ReleasePending(rig.ns("a")); err != nil || !pending {
		t.Fatalf("the crashed owner's witness: %v %v", pending, err)
	}
	rig.seedView("b", entry, digest)
	rig.requireViewsLinkImages("b", entry)
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
	rig.requireViewDirs("a", 0)
}

func TestInterruptedStageLeavesAStageThatReclaimRemoves(t *testing.T) {
	rig := newPoolRig(t)
	seedEmptyLegacyView(t, rig, "a")
	if err := os.WriteFile(filepath.Join(rig.pool.imagesPath(), poolStagePrefix+"interrupted"), []byte("partial image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := stageNames(rig); len(got) != 1 || len(rig.images()) != 0 {
		t.Fatalf("stages %v, images %v; want one abandoned stage and no image", got, rig.images())
	}
	// The abandoned stage never matches a digest, and the scavenger of the task
	// directory never sees it: it lives under the pool, not the task tree.
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
	rig.requireImagesDir(0)
}

func TestInterruptedImageLinkLeavesAnImageThatReclaimPreservesWhenLinked(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "crash-image", "0000000000000000000000000000000000000000000000000000000000000005")
	seedEmptyLegacyView(t, rig, "a")
	if err := os.WriteFile(rig.pool.imagePath(digest), []byte("historical pool fixture data\n"), 0o500); err != nil {
		t.Fatal(err)
	}
	if got := rig.images(); len(got) != 1 {
		t.Fatalf("images %v; want the published image", got)
	}
	// A surviving historical owner still links the image, so reclaim must preserve it.
	rig.seedView("b", entry, digest)
	rig.requireViewsLinkImages("b", entry)
	if got := stageNames(rig); len(got) != 0 {
		t.Fatalf("historical layout left stages %v", got)
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
	rig.requireViewsLinkImages("b", entry)
}

func TestInterruptedFirstViewLinkLeavesAHalfBuiltViewForClosureOwnedRelease(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "crash-view", "0000000000000000000000000000000000000000000000000000000000000008")
	rig.seedView("a", entry, digest)
	if got := rig.viewDirs("a"); len(got) != 1 {
		t.Fatalf("views %v; the crash should leave one half-built view", got)
	}
	if !rig.pending("a") {
		t.Fatal("the crashed owner has no witness")
	}
	// Reclaim never removes a view, so the image a half-built view links stays.
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("ghost")); err != nil {
		t.Fatal(err)
	}
	if got := rig.images(); len(got) != 1 {
		t.Fatalf("reclaim removed an image a leftover view still links: %v", got)
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
	rig.requireImagesDir(0)
	if got := rig.viewDirs("a"); len(got) != 0 {
		t.Fatalf("views after release: %v", got)
	}
}

func TestCrashDuringReleaseKeepsTheWitnessUntilTheRetryCompletes(t *testing.T) {
	for _, point := range []poolPoint{pointAfterViewRemoved, pointBeforeWitnessRemove} {
		t.Run(string(point), func(t *testing.T) {
			rig := newPoolRig(t)
			first, firstDigest := plainEntry(t, rig.dir, "crash-release-1", "0000000000000000000000000000000000000000000000000000000000000006")
			second, secondDigest := plainEntry(t, rig.dir, "crash-release-2", "0000000000000000000000000000000000000000000000000000000000000007")
			rig.seedView("a", first, firstDigest)
			rig.seedView("a", second, secondDigest)
			outcome := runWorker(t, modeCrashRelease+string(point), rig.pool.root, rig.id, "a")
			if outcome.Exit != exitWorkerCrash {
				t.Fatalf("worker exited %d at %s: %s", outcome.Exit, point, outcome.Stderr)
			}
			if pending, err := rig.pool.ReleasePending(rig.ns("a")); err != nil || !pending {
				t.Fatalf("the witness did not survive a crash at %s: %v %v", point, pending, err)
			}
			// after-view-removed fires after the first of two views;
			// before-witness-remove after both.
			wantViews := 1
			if point == pointBeforeWitnessRemove {
				wantViews = 0
			}
			if got := rig.viewDirs("a"); len(got) != wantViews {
				t.Fatalf("views after the crash at %s: %v; want %d", point, got, wantViews)
			}
			if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if rig.pending("a") {
				t.Fatal("the retry left the witness")
			}
			rig.requireImagesDir(0)
		})
	}
}

func TestInterruptedImageStageIsOutsideTheTaskDirectoryScavenger(t *testing.T) {
	rig := newPoolRig(t)
	seedEmptyLegacyView(t, rig, "a")
	if err := os.WriteFile(filepath.Join(rig.pool.imagesPath(), poolStagePrefix+"interrupted"), []byte("partial image"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The task directory scavenger deletes names shaped stage.<id>.tmp; a pool
	// stage must never take that shape.
	for _, name := range stageNames(rig) {
		if strings.HasPrefix(name, "stage.") && strings.HasSuffix(name, ".tmp") {
			t.Fatalf("stage name %q matches the taskdir scavenger's stage pattern", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(rig.pool.root, poolGateName)); err != nil {
		t.Fatal(err)
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
}

func seedEmptyLegacyView(t *testing.T, rig *poolRig, owner string) {
	t.Helper()
	witness := rig.ownerDir(owner)
	if err := os.MkdirAll(witness, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(witness, "delegation-layer-verified-interrupted"), 0o500); err != nil {
		t.Fatal(err)
	}
}
