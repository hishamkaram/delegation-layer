package verifiedexec

// Strict legacy markers and interrupted identity-stage disposal.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

var errInjectedMarkerFault = errors.New("injected marker fault")

// corruptMarker is a marker the strict reader must refuse; plant replaces the
// valid marker of a prepared root.
type corruptMarker struct {
	name  string
	plant func(t *testing.T, root, validID string)
}

func corruptMarkerCases() []corruptMarker {
	content := func(name string, raw string, mode os.FileMode) corruptMarker {
		return corruptMarker{name, func(t *testing.T, root, _ string) { plantMarker(t, root, []byte(raw), mode) }}
	}
	valid := func(id string) string { return `{"version":1,"pool_id":"` + id + `"}` }
	hex := strings.Repeat("c", 64)
	return []corruptMarker{
		content("empty", "", 0o600),
		content("whitespace", ` {"version":1,"pool_id":"`+hex+`"}`, 0o600),
		content("trailing newline", valid(hex)+"\n", 0o600),
		content("unknown field", `{"version":1,"pool_id":"`+hex+`","boot":"x"}`, 0o600),
		content("reordered keys", `{"pool_id":"`+hex+`","version":1}`, 0o600),
		content("duplicate key", `{"version":1,"version":1,"pool_id":"`+hex+`"}`, 0o600),
		content("other version", `{"version":2,"pool_id":"`+hex+`"}`, 0o600),
		content("uppercase id", valid(strings.Repeat("C", 64)), 0o600),
		content("short id", valid("abc"), 0o600),
		content("oversized", valid(hex)+strings.Repeat(" ", 2*maxPoolIdentityBytes), 0o600),
		content("group readable", valid(hex), 0o640),
		content("world readable", valid(hex), 0o644),
		content("owner read only", valid(hex), 0o400),
		content("setuid", valid(hex), os.ModeSetuid|0o600),
		{"directory", func(t *testing.T, root, _ string) {
			replaceMarker(t, root, func(path string) error { return os.Mkdir(path, 0o700) })
		}},
		{"fifo", func(t *testing.T, root, _ string) {
			replaceMarker(t, root, func(path string) error { return unix.Mkfifo(path, 0o600) })
		}},
		{"symlink to a valid record", func(t *testing.T, root, validID string) {
			target := filepath.Join(filepath.Dir(root), "record-elsewhere")
			if err := os.WriteFile(target, canonicalMarker(t, validID), 0o600); err != nil {
				t.Fatal(err)
			}
			replaceMarker(t, root, func(path string) error { return os.Symlink(target, path) })
		}},
	}
}

// replaceMarker removes the prepared marker and lets create put another kind of
// entry at its name.
func replaceMarker(t *testing.T, root string, create func(path string) error) {
	t.Helper()
	path := filepath.Join(root, poolIdentityName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := create(path); err != nil {
		t.Fatal(err)
	}
}

// createExclusive creates a new regular file and fails if the name already
// exists, so a fixture name that aliases another one is an error, never a rewrite.
func createExclusive(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(content)
	return errors.Join(writeErr, file.Close())
}

// TestSweepRemovesOnlyRecognizedAbandonedStages: the grammar is a fixed prefix
// and exactly 32 lowercase hex digits. Everything else, and a recognized name
// that is not a plain file of this user, is preserved and reported.
func TestSweepRemovesOnlyRecognizedAbandonedStages(t *testing.T) {
	rig := newPoolRig(t)
	pool := rig.pool
	nonce := strings.Repeat("a", identityStageNonceLen)
	recognized := poolIdentityStagePrefix + nonce
	preserved := []string{
		poolIdentityStagePrefix + nonce[:31],
		// Distinct from every other name even on a case-insensitive volume, where
		// the uppercase spelling of the recognized lowercase nonce is the same entry.
		poolIdentityStagePrefix + strings.Repeat("E", identityStageNonceLen),
		poolIdentityStagePrefix + nonce + "x",
		poolIdentityName + ".bak",
		poolIdentityStagePrefix + strings.Repeat("b", identityStageNonceLen), // a directory
		poolIdentityStagePrefix + strings.Repeat("c", identityStageNonceLen), // a symlink
	}
	outside := filepath.Join(filepath.Dir(pool.root), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range append([]string{recognized}, preserved[:4]...) {
		if err := createExclusive(filepath.Join(pool.root, name), []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(pool.root, preserved[4]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pool.root, preserved[5])); err != nil {
		t.Fatal(err)
	}
	pool.maintain(context.Background(), true)
	_, _, err := pool.reclaimIdle(context.Background())
	if !errors.Is(err, errPoolIdentityStage) {
		t.Fatalf("reclaim gave %v; want invalid stage refusal", err)
	}
	names := rootNames(t, pool.root)
	if slices.Contains(names, recognized) {
		t.Fatalf("the abandoned stage %s survived: %v", recognized, names)
	}
	for _, name := range preserved {
		if !slices.Contains(names, name) {
			t.Fatalf("the sweep removed %s, which is not a recognized stage: %v", name, names)
		}
	}
	if raw, readErr := os.ReadFile(outside); readErr != nil || string(raw) != "keep" {
		t.Fatalf("the sweep reached through a symlink: %q %v", raw, readErr)
	}
	if !logContains(rig.logged(), "idle image reclaim failed") {
		t.Fatalf("the unremovable recognized names were not reported: %q", rig.logged())
	}
}

// TestIdleReclaimSweepsMarkerStagesOnlyWhileHoldingTheGate: a live publisher
// holds the exclusive gate from before its stage exists, so a reclaim that
// cannot take the gate must leave every stage alone.
func TestIdleReclaimSweepsMarkerStagesOnlyWhileHoldingTheGate(t *testing.T) {
	rig := newPoolRig(t)
	stage := filepath.Join(rig.pool.root, poolIdentityStagePrefix+strings.Repeat("d", identityStageNonceLen))
	if err := os.WriteFile(stage, canonicalMarker(t, rig.id), 0o600); err != nil {
		t.Fatal(err)
	}
	release := holdGate(t, rig.pool)
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("ghost")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stage); err != nil {
		t.Fatalf("reclaim removed the stage of a live publisher: %v", err)
	}
	if !logContains(rig.logged(), "gate busy") {
		t.Fatalf("the busy gate was not reported: %q", rig.logged())
	}
	release()
	result, busy, err := rig.pool.reclaimIdle(context.Background())
	if err != nil || busy || result.IdentityStages != 1 {
		t.Fatalf("reclaim with the gate free gave %+v busy=%v err=%v; want the one abandoned marker stage swept", result, busy, err)
	}
	requireNoIdentityStage(t, rig.pool.root)
	if got := markerID(t, rig.pool.root); got != rig.id {
		t.Fatalf("reclaim changed the identity to %q", got)
	}
}

func TestStageCleanupFaultsAreLoggedWithoutChangingReleaseAndRetry(t *testing.T) {
	for _, point := range []poolPoint{pointMarkerStageRemove, pointMarkerCleanBarrier} {
		t.Run(string(point), func(t *testing.T) {
			checkStageCleanupFault(t, point)
		})
	}
}

func TestReleaseSweepsLegacyIdentityTwinWithoutChangingMarker(t *testing.T) {
	rig := newPoolRig(t)
	marker := filepath.Join(rig.pool.root, poolIdentityName)
	before, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(rig.pool.root, poolIdentityStagePrefix+strings.Repeat("e", identityStageNonceLen))
	if err = os.Link(marker, stage); err != nil {
		t.Fatal(err)
	}
	if err = rig.pool.ReleaseOwner(context.Background(), rig.ns("ghost")); err != nil {
		t.Fatal(err)
	}
	requireNoIdentityStage(t, rig.pool.root)
	after, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || markerID(t, rig.pool.root) != rig.id {
		t.Fatal("removing the interrupted stage changed the canonical marker")
	}
}

func checkStageCleanupFault(t *testing.T, point poolPoint) {
	t.Helper()
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "stage-fault", "0000000000000000000000000000000000000000000000000000000000000019")
	rig.seedView("a", entry, digest)
	stage := filepath.Join(rig.pool.root, poolIdentityStagePrefix+strings.Repeat("d", identityStageNonceLen))
	if err := createExclusive(stage, canonicalMarker(t, rig.id)); err != nil {
		t.Fatal(err)
	}
	reset := failAt(rig.pool, point, errInjectedMarkerFault)
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatalf("stage cleanup changed the release outcome: %v", err)
	}
	if rig.pending("a") {
		t.Fatal("release kept the witness")
	}
	if !logContains(rig.logged(), "idle image reclaim failed") {
		t.Fatalf("cleanup fault not logged: %v", rig.logged())
	}
	if point == pointMarkerStageRemove && len(identityStages(t, rig.pool.root)) != 1 {
		t.Fatal("failed removal lost the stage")
	}
	reset()
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
		t.Fatal(err)
	}
	requireNoIdentityStage(t, rig.pool.root)
	if got := markerID(t, rig.pool.root); got != rig.id {
		t.Fatalf("identity changed to %s", got)
	}
}
