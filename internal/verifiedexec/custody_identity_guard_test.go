package verifiedexec

// The guard: a pool bound to an identity proves that the canonical private root
// still holds that marker before it creates or removes anything, and again at
// the points where a racing change would otherwise be acted on. Any failure to
// prove it leaves the pool unresolved with the concrete cause and changes
// nothing: no release is reported, no identity is reselected, no root repaired.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// requireUnresolved drives every guarded operation against the rig's damaged
// pool and requires each to refuse with the unverified-identity error and the
// cause, leaving the root byte-for-byte unchanged and running no maintenance.
func requireUnresolved(t *testing.T, rig *poolRig, entry, digest string, cause error) {
	t.Helper()
	before := treeSnapshot(t, rig.pool.root)
	ctx := context.Background()
	check := func(operation string, err error) {
		t.Helper()
		if !errors.Is(err, errPoolIdentityUnverified) || (cause != nil && !errors.Is(err, cause)) {
			t.Fatalf("%s gave %v; want an unverified identity caused by %v", operation, err, cause)
		}
	}
	check("ReleaseOwner", rig.pool.ReleaseOwner(ctx, rig.ns("a")))
	pending, pendingErr := rig.pool.ReleasePending(rig.ns("a"))
	check("ReleasePending", pendingErr)
	if pending {
		t.Fatal("ReleasePending answered true alongside a fault")
	}
	_, _, reclaimErr := rig.pool.reclaimIdle(ctx)
	check("reclaim", reclaimErr)
	if after := treeSnapshot(t, rig.pool.root); after != before {
		t.Fatalf("a refused operation changed the root:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if logs := rig.logged(); len(logs) != 0 {
		t.Fatalf("a refused operation ran maintenance: %q", logs)
	}
}

// boundRig is a prepared rig whose owner "a" has a view, so a wrongly
// permitted release or reclaim would visibly remove something.
func boundRig(t *testing.T) (rig *poolRig, entry, digest string) {
	t.Helper()
	rig = newPoolRig(t)
	entry, digest = plainEntry(t, rig.dir, "guard")
	rig.seedView("a", plainRequest(entry, digest))
	return rig, entry, digest
}

func TestGuardedOperationsRefuseAnyMarkerThatIsNotTheExpectedCanonicalOne(t *testing.T) {
	for _, tc := range corruptMarkerCases() {
		t.Run(tc.name, func(t *testing.T) {
			rig, entry, digest := boundRig(t)
			tc.plant(t, rig.pool.root, rig.id)
			requireUnresolved(t, rig, entry, digest, nil)
		})
	}
}

func TestGuardedOperationsRefuseARootThatDoesNotHoldTheExpectedIdentity(t *testing.T) {
	cases := []struct {
		name   string
		damage func(t *testing.T, rig *poolRig)
		cause  error
	}{
		{"marker removed", func(t *testing.T, rig *poolRig) {
			if err := os.Remove(filepath.Join(rig.pool.root, poolIdentityName)); err != nil {
				t.Fatal(err)
			}
		}, fs.ErrNotExist},
		{"another valid identity", func(t *testing.T, rig *poolRig) {
			plantMarker(t, rig.pool.root, canonicalMarker(t, strings.Repeat("e", 64)), 0o600)
		}, errPoolIdentityMismatch},
		{"group-readable root", func(t *testing.T, rig *poolRig) {
			chmodWithRestore(t, rig.pool.root, 0o750)
		}, errPoolRootNotPrivate},
		{"foreign pool at the same path", func(t *testing.T, rig *poolRig) {
			foreign, _ := preparedPool(t, filepath.Join(rig.dir, "foreign"))
			plantMarker(t, foreign.root, canonicalMarker(t, strings.Repeat("b", 64)), 0o600)
			moved := filepath.Join(rig.dir, "moved-original")
			if err := os.Rename(rig.pool.root, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(foreign.root, rig.pool.root); err != nil {
				t.Fatal(err)
			}
		}, errPoolIdentityMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig, entry, digest := boundRig(t)
			tc.damage(t, rig)
			requireUnresolved(t, rig, entry, digest, tc.cause)
		})
	}
}

func chmodWithRestore(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o700); err != nil {
			t.Errorf("restore %s: %v", path, err)
		}
	})
}

// TestGuardedOperationsRefuseARootTheyCannotRead: EACCES stays an unresolved
// pool with its concrete cause. The root is unreadable, so the tree cannot be
// snapshotted; each operation is checked for its cause alone.
func TestGuardedOperationsRefuseARootTheyCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	rig, _, _ := boundRig(t)
	chmodWithRestore(t, rig.pool.root, 0o300)
	ctx := context.Background()

	if err := rig.pool.ReleaseOwner(ctx, rig.ns("a")); !errors.Is(err, unix.EACCES) {
		t.Fatalf("ReleaseOwner gave %v; want EACCES, never success", err)
	}
	if pending, err := rig.pool.ReleasePending(rig.ns("a")); !errors.Is(err, unix.EACCES) || pending {
		t.Fatalf("ReleasePending gave %v %v; want EACCES", pending, err)
	}
	if logs := rig.logged(); len(logs) != 0 {
		t.Fatalf("a refused operation ran maintenance: %q", logs)
	}
}

// TestGuardedOperationsNeverRecreateAMissingRoot: a removed pool is unresolved,
// not an empty pool to be rebuilt, and never "already released".
func TestGuardedOperationsNeverRecreateAMissingRoot(t *testing.T) {
	rig, _, _ := boundRig(t)
	if err := unsealTemporaryDirectory(rig.pool.root); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(rig.pool.root); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := rig.pool.ReleaseOwner(ctx, rig.ns("a")); !errors.Is(err, errPoolIdentityUnverified) {
		t.Fatalf("ReleaseOwner of a removed pool gave %v; want it unresolved, not released", err)
	}
	if pending, err := rig.pool.ReleasePending(rig.ns("a")); err == nil || pending {
		t.Fatalf("ReleasePending of a removed pool gave %v %v; want a concrete fault", pending, err)
	}
	if _, err := os.Lstat(rig.pool.root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a guarded operation recreated the removed root: %v", err)
	}
}

// removeRootOnFirstProof makes the root vanish right after the first identity
// proof, as a racing removal would.
func removeRootOnFirstProof(t *testing.T, rig *poolRig) {
	t.Helper()
	calls := 0
	rig.pool.faults.at = func(point poolPoint, _ string) {
		if point != pointIdentityChecked {
			return
		}
		if calls++; calls == 1 {
			if err := unsealTemporaryDirectory(rig.pool.root); err != nil {
				t.Error(err)
			}
			if err := os.RemoveAll(rig.pool.root); err != nil {
				t.Error(err)
			}
		}
	}
}

// TestRootRemovedAfterTheGuardIsNeverReportedAsReleased: the owner's witness is
// absent only because the whole pool is. Neither answer may read as "released".
func TestRootRemovedAfterTheGuardIsNeverReportedAsReleased(t *testing.T) {
	t.Run("ReleaseOwner", func(t *testing.T) {
		rig, _, _ := boundRig(t)
		removeRootOnFirstProof(t, rig)
		if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); !errors.Is(err, errPoolIdentityUnverified) {
			t.Fatalf("ReleaseOwner gave %v; want it unresolved, never nil", err)
		}
	})
	t.Run("ReleasePending", func(t *testing.T) {
		rig, _, _ := boundRig(t)
		removeRootOnFirstProof(t, rig)
		if pending, err := rig.pool.ReleasePending(rig.ns("a")); !errors.Is(err, errPoolIdentityUnverified) || pending {
			t.Fatalf("ReleasePending gave %v %v; want it unresolved, never false nil", pending, err)
		}
	})
}

func idleImageRig(t *testing.T) (rig *poolRig, digest string) {
	t.Helper()
	rig = newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "idle")
	command := rig.seedView("a", plainRequest(entry, digest))
	if err := unsealTemporaryDirectory(command.temporaryDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(rig.ownerDir("a")); err != nil {
		t.Fatal(err)
	}
	return rig, digest
}

func TestGuardedReclaimRemovesAnIdleImageOfAProvenPool(t *testing.T) {
	rig, digest := idleImageRig(t)
	result, busy, err := rig.pool.reclaimIdle(context.Background())
	if err != nil || busy || result.Images != 1 || rig.imageExists(digest) {
		t.Fatalf("reclaim gave %+v busy=%v err=%v; want the one idle image removed", result, busy, err)
	}
}

// TestGuardedReclaimRefusesAForeignReplacementBeforeDestroyingAnything: the
// marker is replaced after reclaim's first proof and before it holds the gate.
// The proof repeated under the gate refuses, and the idle image stays.
func TestGuardedReclaimRefusesAForeignReplacementBeforeDestroyingAnything(t *testing.T) {
	rig, digest := idleImageRig(t)
	calls := 0
	var replaced string
	rig.pool.faults.at = func(point poolPoint, _ string) {
		if point != pointIdentityChecked {
			return
		}
		if calls++; calls == 1 {
			plantMarker(t, rig.pool.root, canonicalMarker(t, strings.Repeat("f", 64)), 0o600)
			replaced = treeSnapshot(t, rig.pool.root)
		}
	}
	_, _, err := rig.pool.reclaimIdle(context.Background())
	if !errors.Is(err, errPoolIdentityMismatch) {
		t.Fatalf("reclaim gave %v; want %v", err, errPoolIdentityMismatch)
	}
	if !rig.imageExists(digest) {
		t.Fatal("reclaim destroyed an image of a pool whose identity changed")
	}
	if after := treeSnapshot(t, rig.pool.root); after != replaced {
		t.Fatalf("reclaim changed a foreign pool:\nbefore:\n%s\nafter:\n%s", replaced, after)
	}
}

func TestRepeatedReleaseOfAProvenPoolIsIdempotent(t *testing.T) {
	rig, _, _ := boundRig(t)
	for attempt := range 3 {
		if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
			t.Fatalf("release %d: %v", attempt, err)
		}
		if rig.pending("a") {
			t.Fatalf("owner still pending after release %d", attempt)
		}
	}
	rig.requireImagesDir(0)
}
