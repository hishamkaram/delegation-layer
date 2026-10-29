package verifiedexec

// Faults of the identity protocol, injected at the production operations: every
// barrier failure withholds the acknowledgement, a retry repairs the one
// authoritative marker, and cleanup of stages never changes the identity.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

var errInjectedMarkerFault = errors.New("injected marker fault")

func TestPrepareFaultsWithholdTheAcknowledgementAndARetryRepairs(t *testing.T) {
	cases := []struct {
		point     poolPoint
		published bool
	}{
		{pointMarkerStageBarrier, false},
		{pointMarkerLink, false},
		{pointMarkerFileBarrier, true},
		{pointMarkerDirBarrier, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.point), func(t *testing.T) {
			pool, _ := unboundPool(t)
			reset := failAt(pool, tc.point, errInjectedMarkerFault)
			id, err := pool.Prepare(context.Background())
			if id != "" || !errors.Is(err, errInjectedMarkerFault) {
				t.Fatalf("Prepare gave %q, %v; want no acknowledgement and the whole injected error", id, err)
			}
			if uncertain := errors.Is(err, task.ErrUncertainDurability); uncertain != tc.published {
				t.Fatalf("uncertain durability = %v for %v; want %v", uncertain, err, tc.published)
			}
			_, statErr := os.Lstat(filepath.Join(pool.root, poolIdentityName))
			if published := statErr == nil; published != tc.published {
				t.Fatalf("marker published = %v (%v); want %v", published, statErr, tc.published)
			}
			var winner string
			if tc.published {
				winner = markerID(t, pool.root)
			}
			reset()
			retried, err := pool.Prepare(context.Background())
			if err != nil || (tc.published && retried != winner) || retried != markerID(t, pool.root) {
				t.Fatalf("retry gave %q, %v; want the authoritative marker %q", retried, err, markerID(t, pool.root))
			}
			requireNoIdentityStage(t, pool.root)
		})
	}
}

func TestBarrierFailuresOfAnExistingWinnerAreNotAcknowledged(t *testing.T) {
	for _, point := range []poolPoint{pointMarkerFileBarrier, pointMarkerDirBarrier} {
		t.Run(string(point), func(t *testing.T) {
			pool, _ := unboundPool(t)
			want, err := pool.Prepare(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			reset := failAt(pool, point, errInjectedMarkerFault)
			if id, repairErr := pool.Prepare(context.Background()); id != "" || !errors.Is(repairErr, errInjectedMarkerFault) {
				t.Fatalf("repair gave %q, %v; want no acknowledgement", id, repairErr)
			}
			reset()
			if id, retryErr := pool.Prepare(context.Background()); retryErr != nil || id != want {
				t.Fatalf("retry gave %q, %v; want %q", id, retryErr, want)
			}
		})
	}
}

// TestPrepareAdoptsTheWinnerOfACreateOnceRace: another publisher links its own
// marker between this one's stage and its link. The real link then fails with
// EEXIST and Prepare returns that winner, verified, without overwriting it.
func TestPrepareAdoptsTheWinnerOfACreateOnceRace(t *testing.T) {
	pool, _ := unboundPool(t)
	foreign := strings.Repeat("b", 64)
	pool.faults.inject = func(point poolPoint) error {
		if point == pointMarkerLink {
			plantMarker(t, pool.root, canonicalMarker(t, foreign), 0o600)
		}
		return nil
	}
	id, err := pool.Prepare(context.Background())
	if err != nil || id != foreign {
		t.Fatalf("Prepare gave %q, %v; want the winner %q", id, err, foreign)
	}
	if got := markerID(t, pool.root); got != foreign {
		t.Fatalf("the marker was overwritten with %q", got)
	}
	requireNoIdentityStage(t, pool.root)
}

func TestPrepareRefusesACorruptWinnerWithoutReplacingIt(t *testing.T) {
	pool, _ := unboundPool(t)
	corrupt := []byte(`{"version":1,"pool_id":"short"}`)
	pool.faults.inject = func(point poolPoint) error {
		if point == pointMarkerLink {
			plantMarker(t, pool.root, corrupt, 0o600)
		}
		return nil
	}
	if id, err := pool.Prepare(context.Background()); id != "" || !errors.Is(err, errPoolIdentityRecord) {
		t.Fatalf("Prepare gave %q, %v; want %v", id, err, errPoolIdentityRecord)
	}
	if raw, err := os.ReadFile(filepath.Join(pool.root, poolIdentityName)); err != nil || string(raw) != string(corrupt) {
		t.Fatalf("the corrupt winner was changed to %q (%v)", raw, err)
	}
}

// TestPrepareReportsAReadbackFailureAfterTheLink: the marker is replaced by a
// corrupt record after it was linked and before the strict read-back. The
// failure is concrete; it is never hidden as cleanup and never acknowledged.
func TestPrepareReportsAReadbackFailureAfterTheLink(t *testing.T) {
	pool, _ := unboundPool(t)
	pool.faults.inject = func(point poolPoint) error {
		if point == pointMarkerDirBarrier {
			plantMarker(t, pool.root, []byte("not json"), 0o600)
		}
		return nil
	}
	if id, err := pool.Prepare(context.Background()); id != "" || !errors.Is(err, errPoolIdentityRecord) {
		t.Fatalf("Prepare gave %q, %v; want the concrete readback fault", id, err)
	}
}

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

func TestPrepareRefusesAnExistingMarkerThatIsNotStrictlyCanonical(t *testing.T) {
	for _, tc := range corruptMarkerCases() {
		t.Run(tc.name, func(t *testing.T) {
			pool, _ := unboundPool(t)
			id, err := pool.Prepare(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			tc.plant(t, pool.root, id)
			before := treeSnapshot(t, pool.root)
			again, err := pool.Prepare(context.Background())
			if again != "" || err == nil {
				t.Fatalf("Prepare accepted a %s marker: %q, %v", tc.name, again, err)
			}
			if after := treeSnapshot(t, pool.root); after != before {
				t.Fatalf("Prepare changed a root with a %s marker:\nbefore:\n%s\nafter:\n%s", tc.name, before, after)
			}
		})
	}
}

func TestStageCleanupFaultsAreLoggedAndTheIdentityStillAcknowledged(t *testing.T) {
	for _, point := range []poolPoint{pointMarkerStageRemove, pointMarkerCleanBarrier} {
		t.Run(string(point), func(t *testing.T) {
			pool, log := unboundPool(t)
			reset := failAt(pool, point, errInjectedMarkerFault)
			id, err := pool.Prepare(context.Background())
			if err != nil || id != markerID(t, pool.root) {
				t.Fatalf("Prepare gave %q, %v; want the durable marker %q acknowledged", id, err, markerID(t, pool.root))
			}
			if !logContains(log.logged(), "identity stage cleanup failed") {
				t.Fatalf("the cleanup fault was not logged: %q", log.logged())
			}
			if point == pointMarkerStageRemove && len(identityStages(t, pool.root)) != 1 {
				t.Fatalf("the stage that could not be removed is gone: %v", identityStages(t, pool.root))
			}
			reset()
			if retried, retryErr := pool.Prepare(context.Background()); retryErr != nil || retried != id {
				t.Fatalf("retry gave %q, %v; want %q", retried, retryErr, id)
			}
			requireNoIdentityStage(t, pool.root)
		})
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
	pool, log := unboundPool(t)
	if _, err := pool.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
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
	id, err := pool.Prepare(context.Background())
	if err != nil || id != markerID(t, pool.root) {
		t.Fatalf("Prepare gave %q, %v", id, err)
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
	if !logContains(log.logged(), "identity stage cleanup failed") {
		t.Fatalf("the unremovable recognized names were not reported: %q", log.logged())
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
