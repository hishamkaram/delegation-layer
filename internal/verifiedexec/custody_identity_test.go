package verifiedexec

// Pool identity: Prepare publishes one immutable marker per root, is idempotent
// across processes, and refuses to initialize anything through an alias. The
// shared helpers here serve the fault and guard tests as well.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// testLog collects the bounded failures a Pool logs.
type testLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *testLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (l *testLog) logged() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// unboundPool is a fresh unbound pool below a scratch directory with captured
// logs; nothing exists on disk yet.
func unboundPool(t *testing.T) (*Pool, *testLog) {
	t.Helper()
	return unboundPoolAt(t, filepath.Join(scratch(t), "pool"))
}

func unboundPoolAt(t *testing.T, root string) (*Pool, *testLog) {
	t.Helper()
	pool, err := NewPool(root)
	if err != nil {
		t.Fatal(err)
	}
	log := &testLog{}
	pool.logf = log.logf
	return pool, log
}

// failAt makes the production operation named by point fail with err on every
// attempt until the returned function is called.
func failAt(pool *Pool, point poolPoint, err error) (reset func()) {
	pool.faults.inject = func(got poolPoint) error {
		if got == point {
			return err
		}
		return nil
	}
	return func() { pool.faults.inject = nil }
}

func rootNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

func identityStages(t *testing.T, root string) []string {
	t.Helper()
	var stages []string
	for _, name := range rootNames(t, root) {
		if strings.HasPrefix(name, poolIdentityStagePrefix) {
			stages = append(stages, name)
		}
	}
	return stages
}

// markerID reads the marker with the production strict reader's own decoder.
func markerID(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, poolIdentityName))
	if err != nil {
		t.Fatal(err)
	}
	id, err := decodePoolIdentity(raw)
	if err != nil {
		t.Fatalf("marker is not canonical: %v", err)
	}
	return id
}

// plantMarker writes marker bytes with an exact mode, replacing any marker.
func plantMarker(t *testing.T, root string, content []byte, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, poolIdentityName)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func canonicalMarker(t *testing.T, id string) []byte {
	t.Helper()
	raw, err := encodePoolIdentity(id)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func requireNoIdentityStage(t *testing.T, root string) {
	t.Helper()
	if stages := identityStages(t, root); len(stages) != 0 {
		t.Fatalf("marker stages remain: %v", stages)
	}
}

func TestPrepareCreatesTheLayoutAndTheIdentityOnly(t *testing.T) {
	pool, log := unboundPool(t)
	id, err := pool.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !isLowerHex(id, 64) {
		t.Fatalf("identity %q is not 64 lowercase hex", id)
	}
	want := []string{poolGateName, poolIdentityName, poolImagesDir, poolViewsDir}
	if got := rootNames(t, pool.root); !slices.Equal(got, want) {
		t.Fatalf("root holds %v; want exactly %v", got, want)
	}
	for _, sub := range []string{poolImagesDir, poolViewsDir} {
		if entries, readErr := os.ReadDir(filepath.Join(pool.root, sub)); readErr != nil || len(entries) != 0 {
			t.Fatalf("Prepare put %v in %s (%v)", entries, sub, readErr)
		}
	}
	raw, err := os.ReadFile(filepath.Join(pool.root, poolIdentityName))
	if err != nil || !bytes.Equal(raw, []byte(`{"version":1,"pool_id":"`+id+`"}`)) {
		t.Fatalf("marker holds %q (%v); want the canonical record of %s", raw, err, id)
	}
	info, err := os.Lstat(filepath.Join(pool.root, poolIdentityName))
	if err != nil || info.Mode() != 0o600 {
		t.Fatalf("marker mode %v (%v); want exactly 0600", info.Mode(), err)
	}
	if lines := log.logged(); len(lines) != 0 {
		t.Fatalf("a clean Prepare logged %q", lines)
	}
	other, _ := unboundPool(t)
	otherID, err := other.Prepare(context.Background())
	if err != nil || otherID == id {
		t.Fatalf("a second root got identity %q (%v); want a fresh random one", otherID, err)
	}
}

func TestPrepareIsIdempotentAndNeverRewritesTheMarker(t *testing.T) {
	pool, _ := unboundPool(t)
	first, err := pool.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, pool.root)
	for range 3 {
		fresh, newErr := NewPool(pool.root)
		if newErr != nil {
			t.Fatal(newErr)
		}
		again, prepareErr := fresh.Prepare(context.Background())
		if prepareErr != nil || again != first {
			t.Fatalf("repeated Prepare gave %q, %v; want %q", again, prepareErr, first)
		}
	}
	if after := treeSnapshot(t, pool.root); after != before {
		t.Fatalf("a repeated Prepare changed the root:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestGuardedPoolNeverPreparesAgain(t *testing.T) {
	rig := newPoolRig(t)
	before := treeSnapshot(t, rig.pool.root)
	if id, err := rig.pool.Prepare(context.Background()); !errors.Is(err, errPoolPrepareGuarded) || id != "" {
		t.Fatalf("guarded Prepare gave %q, %v; want %v", id, err, errPoolPrepareGuarded)
	}
	if after := treeSnapshot(t, rig.pool.root); after != before {
		t.Fatalf("a refused guarded Prepare changed the root:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestNewPoolWithIdentityRefusesAnEmptyOrMalformedID(t *testing.T) {
	root := filepath.Join(scratch(t), "pool")
	for _, bad := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 32) + "\n" + strings.Repeat("a", 31)} {
		if pool, err := NewPoolWithIdentity(root, bad); !errors.Is(err, errPoolIdentityInvalid) || pool != nil {
			t.Fatalf("NewPoolWithIdentity(%q) gave %v, %v; want %v and no pool", bad, pool, err, errPoolIdentityInvalid)
		}
	}
	if pool, err := NewPoolWithIdentity("relative", strings.Repeat("a", 64)); !errors.Is(err, errPoolRootInvalid) || pool != nil {
		t.Fatalf("a relative root gave %v, %v; want %v", pool, err, errPoolRootInvalid)
	}
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a constructor touched the disk: %v", err)
	}
}

func TestPrepareRefusesANilOrCancelledContextBeforeAnyMutation(t *testing.T) {
	pool, _ := unboundPool(t)
	var nilContext context.Context
	if id, err := pool.Prepare(nilContext); !errors.Is(err, errNilContext) || id != "" {
		t.Fatalf("nil context gave %q, %v; want %v", id, err, errNilContext)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if id, err := pool.Prepare(cancelled); !errors.Is(err, context.Canceled) || id != "" {
		t.Fatalf("cancelled context gave %q, %v; want %v", id, err, context.Canceled)
	}
	if _, err := os.Lstat(pool.root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused Prepare created the root: %v", err)
	}
}

func TestPrepareWaitsForTheGateOnlyAsLongAsItsContext(t *testing.T) {
	pool, _ := unboundPool(t)
	if _, err := pool.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	holder := holdGate(t, pool)
	defer holder()
	deadline, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer stop()
	before := treeSnapshot(t, pool.root)
	if id, err := pool.Prepare(deadline); !errors.Is(err, context.DeadlineExceeded) || id != "" {
		t.Fatalf("Prepare behind a held gate gave %q, %v; want the context deadline", id, err)
	}
	if after := treeSnapshot(t, pool.root); after != before {
		t.Fatalf("a Prepare that timed out changed the root:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// holdGate takes the pool's exclusive gate the way another live publisher
// would, and returns the function that releases it. It is also registered as a
// cleanup, so an early failure still releases it exactly once.
func holdGate(t *testing.T, pool *Pool) (release func()) {
	t.Helper()
	gate, err := taskdir.OpenLockFileContext(context.Background(), pool.gatePath(), taskdir.LockLevelNone)
	if err != nil {
		t.Fatal(err)
	}
	if err = gate.LockEXNonblocking(); err != nil {
		t.Fatal(errors.Join(err, gate.Close()))
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			if closeErr := errors.Join(gate.Unlock(), gate.Close()); closeErr != nil {
				t.Errorf("release the held gate: %v", closeErr)
			}
		})
	}
	t.Cleanup(release)
	return release
}

func TestPrepareRefusesAnAliasedAncestryBeforeCreatingThroughIt(t *testing.T) {
	for _, tc := range aliasedAncestryCases() {
		t.Run(tc.name, func(t *testing.T) {
			root, outside := tc.build(t, scratch(t))
			before := treeSnapshot(t, outside)
			pool, err := NewPool(root)
			if err != nil {
				t.Fatal(err)
			}
			if id, prepareErr := pool.Prepare(context.Background()); !errors.Is(prepareErr, errPoolRootNotCanon) || id != "" {
				t.Fatalf("Prepare behind an alias gave %q, %v; want %v", id, prepareErr, errPoolRootNotCanon)
			}
			if after := treeSnapshot(t, outside); after != before {
				t.Fatalf("Prepare created through the alias:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestConcurrentProcessesPrepareOneIdentity(t *testing.T) {
	root := filepath.Join(scratch(t), "pool")
	workers := make([]*workerProcess, concurrentActors)
	for i := range workers {
		workers[i] = startWorker(t, modePrepare, root)
	}
	ids := map[string]int{}
	for i, worker := range workers {
		outcome := worker.wait(t)
		if outcome.Exit != 0 {
			t.Fatalf("worker %d exited %d: %s", i, outcome.Exit, outcome.Stderr)
		}
		ids[strings.TrimSpace(string(outcome.Stdout))]++
	}
	if len(ids) != 1 {
		t.Fatalf("workers acknowledged %d identities: %v", len(ids), ids)
	}
	if ids[markerID(t, root)] != concurrentActors {
		t.Fatalf("acknowledged identities %v do not all equal the marker %s", ids, markerID(t, root))
	}
	requireNoIdentityStage(t, root)
	newRecorder(t).note("prepare", "%d processes prepared one root concurrently and acknowledged one identity", concurrentActors)
}

func TestCrashAfterTheLinkLeavesATwinThatTheNextPrepareRepairs(t *testing.T) {
	root := filepath.Join(scratch(t), "pool")
	outcome := runWorker(t, modeCrashPrepare+string(pointMarkerLinked), root)
	if outcome.Exit != exitWorkerCrash {
		t.Fatalf("worker exited %d at %s, want %d: %s", outcome.Exit, pointMarkerLinked, exitWorkerCrash, outcome.Stderr)
	}
	id := markerID(t, root)
	if stages := identityStages(t, root); len(stages) != 1 {
		t.Fatalf("stages %v; the crash should leave the stage as the marker's twin", stages)
	}
	info, err := os.Lstat(filepath.Join(root, poolIdentityName))
	if err != nil {
		t.Fatal(err)
	}
	if links, linkErr := linkCount(info); linkErr != nil || links != 2 {
		t.Fatalf("marker link count %d (%v); want the crash twin's 2", links, linkErr)
	}
	// The bounded strict read accepts the twin: a guarded pool still proves it.
	guarded, err := NewPoolWithIdentity(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = guarded.requireIdentity(); err != nil {
		t.Fatalf("a crash twin with link count 2 was refused: %v", err)
	}
	retry, err := NewPool(root)
	if err != nil {
		t.Fatal(err)
	}
	if again, prepareErr := retry.Prepare(context.Background()); prepareErr != nil || again != id {
		t.Fatalf("retry after the crash gave %q, %v; want the authoritative %q", again, prepareErr, id)
	}
	requireNoIdentityStage(t, root)
	info, err = os.Lstat(filepath.Join(root, poolIdentityName))
	if links, linkErr := linkCount(info); err != nil || linkErr != nil || links != 1 {
		t.Fatalf("marker link count %d after repair (%v %v); want 1", links, err, linkErr)
	}
}
