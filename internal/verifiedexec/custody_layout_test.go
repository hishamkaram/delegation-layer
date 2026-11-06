package verifiedexec

// Read-only layout validation: release, ReleasePending and idle reclaim must
// validate the canonical private chain root -> views -> owner (and root ->
// images) before they observe or remove anything. A moved state directory left
// behind as a symlink must never be followed. The static guard does not claim
// protection against a hostile concurrent replacement of an ancestor.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// treeSnapshot records every entry below root with its mode and, for regular
// files, its digest, without following symlinks. Its inputs contain only literal
// fixture data and canonical marker JSON; never installed executable files.
func treeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		line := path + " " + info.Mode().String()
		if info.Mode().IsRegular() {
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			sum := sha256.Sum256(content)
			line += " " + hex.EncodeToString(sum[:])
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// moveToOutsideAndAlias moves pool/name to outside and leaves a symlink to it,
// as an operator who relocated a state directory would.
func moveToOutsideAndAlias(t *testing.T, rig *poolRig, name string) (outside string) {
	t.Helper()
	outside = filepath.Join(rig.dir, "outside-"+name)
	inside := filepath.Join(rig.pool.root, name)
	if err := os.Rename(inside, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, inside); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unsealTemporaryDirectory(outside); err != nil {
			t.Errorf("unseal outside: %v", err)
		}
	})
	return outside
}

func TestReleaseAndPendingRefuseAnAliasedViewsDirectory(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "views-alias", "000000000000000000000000000000000000000000000000000000000000001e")
	rig.seedView("a", entry, digest)
	outside := moveToOutsideAndAlias(t, rig, poolViewsDir)
	before := treeSnapshot(t, outside)

	if pending, err := rig.pool.ReleasePending(rig.ns("a")); err == nil {
		t.Errorf("ReleasePending answered %v from the tree behind a symlinked views directory", pending)
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err == nil {
		t.Error("ReleaseOwner followed a symlinked views directory and reported success")
	}
	if after := treeSnapshot(t, outside); after != before {
		t.Fatalf("the tree behind the alias changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestReleaseRefusesADanglingViewsAliasWithAConcreteFault(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "views-dangling", "000000000000000000000000000000000000000000000000000000000000001f")
	rig.seedView("a", entry, digest)
	moved := filepath.Join(rig.dir, "gone-views")
	if err := os.Rename(rig.pool.viewsPath(), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(rig.dir, "does-not-exist"), rig.pool.viewsPath()); err != nil {
		t.Fatal(err)
	}
	if pending, err := rig.pool.ReleasePending(rig.ns("a")); err == nil || pending {
		t.Fatalf("ReleasePending over a dangling alias gave %v %v; want a concrete fault", pending, err)
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err == nil {
		t.Fatal("ReleaseOwner treated a dangling alias as a missing namespace")
	}
}

func TestIdleReclaimRefusesAnAliasedImagesDirectory(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "images-alias", "000000000000000000000000000000000000000000000000000000000000000f")
	rig.seedView("a", entry, digest)
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
	outside := moveToOutsideAndAlias(t, rig, poolImagesDir)
	victim := filepath.Join(outside, strings.Repeat("c", 64))
	if err := os.WriteFile(victim, []byte("someone else's file"), poolImageMode); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, outside)
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("ghost")); err != nil {
		t.Fatalf("an unrelated release failed because of a reclaim fault: %v", err)
	}
	if after := treeSnapshot(t, outside); after != before {
		t.Fatalf("reclaim removed or changed files behind a symlinked images directory:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if !logContains(rig.logged(), "idle image reclaim failed") {
		t.Fatalf("the refused reclaim was not logged: %q", rig.logged())
	}
}

func TestReadOnlyOperationsRefuseASymlinkedRoot(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "root-alias", "0000000000000000000000000000000000000000000000000000000000000017")
	rig.seedView("a", entry, digest)
	real := filepath.Join(rig.dir, "real-pool")
	if err := os.Rename(rig.pool.root, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, rig.pool.root); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, real)
	if _, err := rig.pool.ReleasePending(rig.ns("a")); err == nil {
		t.Error("ReleasePending followed a symlinked root")
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err == nil {
		t.Error("ReleaseOwner followed a symlinked root")
	}
	if after := treeSnapshot(t, real); after != before {
		t.Fatalf("the real pool changed through the alias:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestMissingChainLinksAreAbsentAndCreateNothing(t *testing.T) {
	rig := newPoolRig(t)
	ns := rig.ns("a")
	assertAbsent := func(label string) {
		t.Helper()
		if pending, err := rig.pool.ReleasePending(ns); err != nil || pending {
			t.Fatalf("%s: ReleasePending gave %v %v; want false nil", label, pending, err)
		}
		if err := rig.pool.ReleaseOwner(context.Background(), ns); err != nil {
			t.Fatalf("%s: ReleaseOwner gave %v; want success", label, err)
		}
	}
	if err := os.Remove(rig.pool.viewsPath()); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, rig.pool.root)
	assertAbsent("missing views")
	if after := treeSnapshot(t, rig.pool.root); after != before {
		t.Fatalf("a read-only call changed the root:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if err := os.Mkdir(rig.pool.viewsPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	assertAbsent("missing owner")
	if entries, err := os.ReadDir(rig.pool.viewsPath()); err != nil || len(entries) != 0 {
		t.Fatalf("a read-only call created entries below views: %v %v", entries, err)
	}
}

func TestOwnReleaseSucceedsWhenTheImagesDirectoryIsCorrupt(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "corrupt-images", "0000000000000000000000000000000000000000000000000000000000000003")
	rig.seedView("a", entry, digest)
	// Corrupt the images directory after bind: replace it by a regular file.
	images := rig.pool.imagesPath()
	if err := os.Rename(images, images+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(images, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatalf("the owner's own release failed because of unrelated image corruption: %v", err)
	}
	if rig.pending("a") {
		t.Fatal("image corruption kept the released owner's witness")
	}
	if !logContains(rig.logged(), "idle image reclaim failed") {
		t.Fatalf("the reclaim fault was not logged separately: %q", rig.logged())
	}
}

func TestReleaseTreatsAnOwnerGoneAtRemovalAsAlreadyReleased(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "owner-gone", "0000000000000000000000000000000000000000000000000000000000000015")
	rig.seedView("a", entry, digest)
	rig.pool.faults.at = func(point poolPoint, _ string) {
		if point == pointBeforeWitnessRemove {
			// A concurrent release of the same, already closed, owner finishes
			// between this release's last view removal and its witness removal.
			if err := os.Remove(rig.ownerDir("a")); err != nil {
				t.Errorf("simulate the concurrent release: %v", err)
			}
		}
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatalf("an owner that disappeared at removal gave %v; want idempotent success", err)
	}
}

func TestReleaseKeepsEveryOtherRemovalFault(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "other-fault", "0000000000000000000000000000000000000000000000000000000000000014")
	rig.seedView("a", entry, digest)
	rig.pool.faults.at = func(point poolPoint, _ string) {
		if point == pointBeforeWitnessRemove {
			// Make removal fail for a reason other than absence.
			if err := os.WriteFile(filepath.Join(rig.ownerDir("a"), "stranger"), nil, 0o600); err != nil {
				t.Errorf("plant: %v", err)
			}
		}
	}
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err == nil {
		t.Fatal("a non-empty witness at removal was reported as released")
	}
	if !rig.pending("a") {
		t.Fatal("the witness did not survive a real removal fault")
	}
}

// poolRootCases are roots that do not exist yet but whose existing ancestry is
// not canonical. A read-only call must refuse them rather than answer "absent".
type aliasedAncestry struct {
	name  string
	build func(t *testing.T, base string) (root, outside string)
}

func aliasedAncestryCases() []aliasedAncestry {
	return []aliasedAncestry{
		{"live ancestor alias", func(t *testing.T, base string) (string, string) {
			outside := filepath.Join(base, "outside")
			mkdirMode(t, outside, 0o700)
			if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("outside bytes"), 0o640); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(base, "alias")
			if err := os.Symlink(outside, alias); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(alias, "pool"), outside
		}},
		{"dangling ancestor alias", func(t *testing.T, base string) (string, string) {
			outside := filepath.Join(base, "outside")
			mkdirMode(t, outside, 0o700)
			dangling := filepath.Join(base, "dangling")
			if err := os.Symlink(filepath.Join(base, "does-not-exist"), dangling); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(dangling, "pool"), outside
		}},
	}
}

func TestReadOnlyCallsRefuseAMissingRootBehindAnAliasedAncestor(t *testing.T) {
	for _, tc := range aliasedAncestryCases() {
		t.Run(tc.name, func(t *testing.T) {
			base := scratch(t)
			root, outside := tc.build(t, base)
			before := treeSnapshot(t, outside)
			pool, err := NewPoolWithIdentity(root, strings.Repeat("a", 64))
			if err != nil {
				t.Fatal(err)
			}
			ns := mustNamespace(t, "a")
			if pending, pendingErr := pool.ReleasePending(ns); pendingErr == nil {
				t.Errorf("ReleasePending answered %v for a root behind an aliased ancestor", pending)
			}
			if releaseErr := pool.ReleaseOwner(context.Background(), ns); releaseErr == nil {
				t.Error("ReleaseOwner reported success for a root behind an aliased ancestor")
			}
			if after := treeSnapshot(t, outside); after != before {
				t.Fatalf("the tree behind the alias changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestReadOnlyCallsRefuseAMissingRootWithCanonicalAncestorsAndCreateNothing(t *testing.T) {
	base := scratch(t)
	root := filepath.Join(base, "not", "yet", "created", "pool")
	pool, err := NewPoolWithIdentity(root, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	ns := mustNamespace(t, "a")
	if pending, pendingErr := pool.ReleasePending(ns); !errors.Is(pendingErr, fs.ErrNotExist) || pending {
		t.Fatalf("ReleasePending gave %v %v; want a missing-root fault", pending, pendingErr)
	}
	if err = pool.ReleaseOwner(context.Background(), ns); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReleaseOwner gave %v; want a missing-root fault, never success", err)
	}
	if _, err = os.Lstat(filepath.Join(base, "not")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a read-only call created the missing ancestors: %v", err)
	}
}
