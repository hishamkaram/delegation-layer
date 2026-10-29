package verifiedexec

// Crash fixtures: a worker process exits at a named pool point; the parent
// waits for that exact process and then inspects what a real crash left. What
// remains is closure-owned: only ReleaseOwner, called after canonical closure,
// removes it.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func crashBind(t *testing.T, rig *poolRig, point poolPoint, owner string, entry, digest string) {
	t.Helper()
	outcome := runWorker(t, modeCrashBind+string(point), rig.pool.root, rig.id, owner, entry, digest)
	if outcome.Exit != exitWorkerCrash {
		t.Fatalf("worker exited %d at %s, want %d: %s", outcome.Exit, point, exitWorkerCrash, outcome.Stderr)
	}
}

func stageNames(rig *poolRig) []string {
	var stages []string
	for _, name := range rig.imagesDirEntries() {
		if strings.HasPrefix(name, poolStagePrefix) {
			stages = append(stages, name)
		}
	}
	return stages
}

func TestCrashBeforeCopyLeavesTheWitnessAndAnEmptyView(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "crash-before-copy")
	crashBind(t, rig, pointBeforeCopy, "a", entry, digest)
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
	rig.binder("b", plainRequest(entry, digest))
	rig.requireViewsLinkImages("b", entry)
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
	rig.requireViewDirs("a", 0)
}

func TestCrashAfterStageLeavesAStageThatReclaimRemoves(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "crash-stage")
	crashBind(t, rig, pointAfterStage, "a", entry, digest)
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

func TestCrashAfterImageLinkLeavesAVerifiableImage(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "crash-image")
	crashBind(t, rig, pointAfterImageLink, "a", entry, digest)
	if got := rig.images(); len(got) != 1 {
		t.Fatalf("images %v; want the published image", got)
	}
	// A later bind verifies the leftover image again before linking it.
	rig.binder("b", plainRequest(entry, digest))
	rig.requireViewsLinkImages("b", entry)
	if got := stageNames(rig); len(got) != 0 {
		t.Fatalf("a later bind left stages %v", got)
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
	rig.requireViewsLinkImages("b", entry)
}

func TestCrashAfterFirstViewLinkLeavesAHalfBuiltViewForClosureOwnedRelease(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "crash-view")
	crashBind(t, rig, pointAfterViewLink, "a", entry, digest)
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
			first, firstDigest := plainEntry(t, rig.dir, "crash-release-1")
			second, secondDigest := plainEntry(t, rig.dir, "crash-release-2")
			outcome := runWorker(t, modeCrashRelease+string(point), rig.pool.root, rig.id, "a", first, firstDigest, second, secondDigest)
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

func TestCrashLeavesNothingTheTaskDirectoryScavengerOwns(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "crash-names")
	crashBind(t, rig, pointAfterStage, "a", entry, digest)
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
