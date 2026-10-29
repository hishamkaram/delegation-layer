package verifiedexec

// Concurrency: many binders and releasers in one process and in separate
// processes against one pool.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

const concurrentActors = 8

func TestPoolCreateOnceRaceCopiesOnce(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "race")
	var mu sync.Mutex
	copies := 0
	// Every binder is held at the point where it has seen the image missing under
	// the shared gate and is about to ask for the exclusive one, so the only thing
	// that can keep the copies at one is the recheck under the exclusive gate.
	var arrived sync.WaitGroup
	arrived.Add(concurrentActors)
	released := make(chan struct{})
	go func() {
		arrived.Wait()
		close(released)
	}()
	rig.pool.faults.at = func(point poolPoint, _ string) {
		if point == pointBeforeCopy {
			mu.Lock()
			copies++
			mu.Unlock()
		}
		if point == pointBeforeExclusive {
			arrived.Done()
			select {
			case <-released:
			case <-time.After(handshakeTimeout):
				t.Error("the binders never all reached the exclusive gate")
			}
		}
	}
	start := make(chan struct{})
	errs := make([]error, concurrentActors)
	commands := make([]*Command, concurrentActors)
	var wg sync.WaitGroup
	for i := range concurrentActors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			commands[i], errs[i] = rig.pool.Bind(context.Background(), rig.ns("o"+strconv.Itoa(i)), plainRequest(entry, digest))
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("binder %d: %v", i, err)
		}
		if closeErr := commands[i].Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if copies != 1 {
		t.Fatalf("%d copies for %d binders of one new digest; want exactly 1", copies, concurrentActors)
	}
	rig.requireImagesDir(1)
	for i := range concurrentActors {
		rig.requireViewsLinkImages("o"+strconv.Itoa(i), entry)
	}
	info, err := os.Lstat(rig.pool.imagePath(digest))
	if err != nil {
		t.Fatal(err)
	}
	if count, countErr := linkCount(info); countErr != nil || count != concurrentActors+1 {
		t.Fatalf("link count %d (%v); want %d", count, countErr, concurrentActors+1)
	}
	rig.rec.note("create-once-race", "%d goroutines, %d copy, one image, link count %d", concurrentActors, copies, concurrentActors+1)
}

// TestPoolBindVersusReleaseAndReclaim runs different owners that bind and
// release the same digests while a stable owner stays bound. Each bound view
// must hold the admitted bytes at the moment Bind returns; reclaim must never
// remove an image a live view links, and the stable owner must survive.
func TestPoolBindVersusReleaseAndReclaim(t *testing.T) {
	rig := newPoolRig(t)
	entries := make([]string, 2)
	digests := make([]string, 2)
	for i := range entries {
		entries[i], digests[i] = plainEntry(t, rig.dir, fmt.Sprintf("churn-%d", i))
	}
	rig.binder("stable", plainRequest(entries[0], digests[0]))

	const iterations = 25
	errs := make([]error, concurrentActors)
	var wg sync.WaitGroup
	for actor := range concurrentActors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[actor] = churnGoroutine(rig, actor, iterations, entries, digests)
		}()
	}
	wg.Wait()
	for actor, err := range errs {
		if err != nil {
			t.Fatalf("actor %d: %v", actor, err)
		}
	}
	rig.requireViewsLinkImages("stable", entries[0])
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("stable")); err != nil {
		t.Fatal(err)
	}
	rig.requireImagesDir(0)
	rig.rec.note("bind-vs-release", "%d goroutines x %d bind/release cycles over %d digests; the stable owner kept its image; the pool ended empty", concurrentActors, iterations, len(digests))
}

func churnGoroutine(rig *poolRig, actor, iterations int, entries, digests []string) error {
	ctx := context.Background()
	for i := range iterations {
		which := (actor + i) % len(entries)
		ns, err := NewNamespace(fmt.Sprintf("g%d-%d", actor, i))
		if err != nil {
			return err
		}
		command, err := rig.pool.Bind(ctx, ns, plainRequest(entries[which], digests[which]))
		if err != nil {
			return fmt.Errorf("bind %s: %w", ns, err)
		}
		if err = errors.Join(requireViewHoldsDigest(command, digests[which]), command.Close()); err != nil {
			return err
		}
		if err = rig.pool.ReleaseOwner(ctx, ns); err != nil {
			return fmt.Errorf("release %s: %w", ns, err)
		}
	}
	return nil
}

func TestPoolBindVersusReleaseAcrossProcesses(t *testing.T) {
	dir := scratch(t)
	pool, id := preparedPool(t, dir+"/pool")
	root := pool.root
	entry, digest := plainEntry(t, dir, "processes")
	other, otherDigest := plainEntry(t, dir, "processes-other")
	workers := make([]*workerProcess, concurrentActors)
	for i := range workers {
		workers[i] = startWorker(t, modeChurn, root, id, "p"+strconv.Itoa(i)+"-", "15", entry, digest, other, otherDigest)
	}
	for i, worker := range workers {
		outcome := worker.wait(t)
		if outcome.Exit != 0 {
			t.Fatalf("worker %d exited %d: %s", i, outcome.Exit, outcome.Stderr)
		}
	}
	// One uncontended release lets the best-effort reclaim finish what a busy
	// gate skipped during the race.
	if err := pool.ReleaseOwner(context.Background(), mustNamespace(t, "final")); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{poolImagesDir, poolViewsDir} {
		entries, readErr := os.ReadDir(root + "/" + sub)
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("%s after all workers released: %v %v", sub, entries, readErr)
		}
	}
	newRecorder(t).note("processes", "%d worker processes x 15 bind/release cycles; nothing left behind", concurrentActors)
}

func TestPoolBindVersusReleaseOfTheSameOwnerAfterClosure(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := plainEntry(t, rig.dir, "same-owner")
	for cycle := range 20 {
		command := rig.binder("a", plainRequest(entry, digest))
		if err := requireViewHoldsDigest(command, digest); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("a")); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	rig.requireImagesDir(0)
}
