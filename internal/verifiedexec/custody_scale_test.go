package verifiedexec

// Scale: 200 simultaneously live finite owned readers of one tiny native image,
// and a large read-only image copied once and linked after. These are metadata
// and kernel-reported facts about this host and run, not claims about shared
// RAM (PSS), real providers, or other machines.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	readerCount         = 200
	readerBinders       = 8
	readerReadyDeadline = 15 * time.Second
	largeBudgetBytes    = 1 << 30
)

// requireDescriptorCapacity reports a setup blocker instead of changing a host
// limit: the parent holds a stdin writer per child, and Linux adds a pidfd.
func requireDescriptorCapacity(t *testing.T, children int) {
	t.Helper()
	limit, err := descriptorLimit()
	if err != nil {
		t.Fatalf("setup blocked: read descriptor limit: %v", err)
	}
	if need := uint64(openDescriptorsOrFail(t)) + 2*uint64(children) + 64; limit < need {
		t.Fatalf("setup blocked: RLIMIT_NOFILE soft limit %d < %d needed for %d live children; this proof never changes host limits", limit, need, children)
	}
}

func bindReaders(t *testing.T, rig *poolRig, entry, digest string) []*Command {
	t.Helper()
	commands := make([]*Command, readerCount)
	var wg sync.WaitGroup
	errs := make([]error, readerBinders)
	for b := range readerBinders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := b; i < readerCount; i += readerBinders {
				ns, err := NewNamespace("r" + strconv.Itoa(i))
				if err != nil {
					errs[b] = err
					return
				}
				commands[i], err = rig.pool.Bind(context.Background(), ns, CommandRequest{Executable: entry, SHA256: digest, Args: []string{helperArg, "handshake"}})
				if err != nil {
					errs[b] = err
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("bind readers: %v", err)
	}
	return commands
}

func awaitReady(t *testing.T, outPath string, want int) {
	t.Helper()
	deadline := time.After(readerReadyDeadline)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		content, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Count(content, []byte("READY\n")) >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("only %d of %d readers announced READY within %s", bytes.Count(content, []byte("READY\n")), want, readerReadyDeadline)
		case <-ticker.C:
		}
	}
}

func requireReaderReports(t *testing.T, outPath, digest string, want int) {
	t.Helper()
	content, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, line := range bytes.Split(content, []byte("\n")) {
		if len(line) == 0 || bytes.Equal(line, []byte("READY")) {
			continue
		}
		report, parseErr := parseReport(line)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if report.Marker != digest || report.Handshake != "go" {
			t.Fatalf("reader report %+v; want the admitted bytes after go", report)
		}
		seen[report.Executable] = true
	}
	if len(seen) != want {
		t.Fatalf("%d distinct views reported by %d readers", len(seen), want)
	}
}

func TestPool200ReadersShareOneImage(t *testing.T) {
	rig := newPoolRig(t)
	entry, digest := selfNative(t, filepath.Join(rig.dir, "readers"))
	requireDescriptorCapacity(t, readerCount)
	before := openDescriptorsOrFail(t)
	limit, err := descriptorLimit()
	if err != nil {
		t.Fatal(err)
	}
	rig.rec.note("descriptors-before", "open=%d soft-limit=%d", before, limit)

	started := time.Now()
	commands := bindReaders(t, rig, entry, digest)
	bound := time.Since(started)
	outPath := filepath.Join(rig.dir, "readers.out")
	fleet := newReaderFleet(t)
	if err = fleet.startAll(commands, outPath); err != nil {
		t.Fatalf("start readers: %v", err)
	}
	awaitReady(t, outPath, readerCount)
	live := time.Since(started)

	peak := openDescriptorsOrFail(t)
	storage, err := measureTrees(rig.dir + "/pool")
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.Lstat(rig.pool.imagePath(digest))
	if err != nil {
		t.Fatal(err)
	}
	links, err := linkCount(image)
	if err != nil {
		t.Fatal(err)
	}
	if got := rig.images(); len(got) != 1 || links != readerCount+1 {
		t.Fatalf("%d images with link count %d for %d live readers; want one image linked %d times", len(got), links, readerCount, readerCount+1)
	}
	if err = fleet.release(); err != nil {
		t.Fatalf("release readers: %v", err)
	}
	cpu, maxRSS, sumRSS := fleet.totals()
	requireReaderReports(t, outPath, digest, readerCount)
	rig.rec.note("readers", "%d simultaneously live owned children, %d binder goroutines, 1 image, link count %d", readerCount, readerBinders, links)
	rig.rec.note("wall", "bind-200=%s start-to-all-ready=%s total=%s", bound, live-bound, time.Since(started))
	rig.rec.note("descriptors-peak", "open=%d (before %d)", peak, before)
	rig.rec.note("storage-at-peak", "inodes=%d unique-logical=%d allocated=%d sum-of-named-sizes=%d (arithmetic over stat sizes, not a copy measurement)",
		storage.Inodes, storage.UniqueLogical, storage.Allocated, storage.NamedLogical)
	rig.rec.note("children-resources", "cpu-sum=%s max-rss=%d mean-rss=%d bytes (kernel rusage; not PSS; Linux ru_maxrss can carry the parent image across fork, so it is an upper bound, not a clean child peak)", cpu, maxRSS, sumRSS/readerCount)

	// Fixture ordering only: every reader was Waited above, so this release
	// stands in for the canonical closure a real owner would prove.
	for i := range readerCount {
		if err = rig.pool.ReleaseOwner(context.Background(), rig.ns("r"+strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	rig.requireImagesDir(0)
	rig.rec.note("descriptors-after", "open=%d", openDescriptorsOrFail(t))
}

// padded writes head followed by zeros up to size bytes as an executable.
func padded(t *testing.T, path string, head []byte, size int64) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	_, headErr := file.Write(head)
	_, padErr := io.CopyN(file, zeroReader{}, size-int64(len(head)))
	if err = errors.Join(headErr, padErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	return fileDigest(t, path)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func requireFreeSpace(t *testing.T, dir string, need uint64) {
	t.Helper()
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		t.Fatalf("setup blocked: statfs %s: %v", dir, err)
	}
	if free := uint64(stat.Bavail) * uint64(stat.Bsize); free < need {
		t.Fatalf("setup blocked: %d bytes free below %s, %d needed", free, dir, need)
	}
}

// largeSizeWithinBudget reports whether a source of size bytes plus one image of
// the same size fits the physical budget. The comparison is done on size before
// any multiplication, so no int64 can wrap.
func largeSizeWithinBudget(size int64) bool {
	return size > 0 && size <= largeBudgetBytes/2
}

func TestPoolLargeImageIsCopiedOnceAndLinkedAfter(t *testing.T) {
	requireScale(t)
	raw := os.Getenv(proofLargeBytesEnv)
	if raw == "" {
		t.Skipf("large-image run not requested: set %s to a size within the %d byte staging budget", proofLargeBytesEnv, largeBudgetBytes/2)
	}
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || !largeSizeWithinBudget(size) {
		t.Fatalf("setup blocked: %s=%q: source plus one image must fit the %d byte physical budget", proofLargeBytesEnv, raw, largeBudgetBytes)
	}
	rig := newPoolRig(t)
	requireFreeSpace(t, rig.dir, uint64(2*size)+256<<20)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	head, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(rig.dir, "fixture-large", "entry-large")
	digest := padded(t, entry, head[:min(len(head), 1<<20)], size)
	rig.rec.note("large-source", "%d bytes sha256=%s budget=%d", size, digest, largeBudgetBytes)

	copies := 0
	rig.pool.faults.at = func(point poolPoint, _ string) {
		if point == pointBeforeCopy {
			copies++
		}
	}
	var previous uint64
	for i, owner := range []string{"a", "b", "c"} {
		started := time.Now()
		rig.binder(owner, plainRequest(entry, digest))
		elapsed := time.Since(started)
		storage, measureErr := measureTrees(rig.pool.root)
		if measureErr != nil {
			t.Fatal(measureErr)
		}
		if storage.Allocated > largeBudgetBytes {
			t.Fatalf("allocated %d bytes exceeds the %d byte staging budget after bind %s", storage.Allocated, largeBudgetBytes, owner)
		}
		if i > 0 && storage.Allocated > previous+1<<20 {
			t.Fatalf("bind %s allocated %d new bytes; a reuse bind must only link", owner, storage.Allocated-previous)
		}
		previous = storage.Allocated
		rig.rec.note("large-bind-"+owner, "wall=%s inodes=%d unique-logical=%d allocated=%d copies=%d", elapsed, storage.Inodes, storage.UniqueLogical, storage.Allocated, copies)
	}
	if copies != 1 {
		t.Fatalf("%d byte copies for three binds of one digest; want 1", copies)
	}
	rig.requireImagesDir(1)
}

func TestLargeSizeBudgetCheckDoesNotOverflow(t *testing.T) {
	for _, tc := range []struct {
		size int64
		want bool
	}{
		{0, false},
		{-1, false},
		{1, true},
		{largeBudgetBytes / 2, true},
		{largeBudgetBytes/2 + 1, false},
		{math.MaxInt64/2 - 1, false},
		{math.MaxInt64 / 2, false},
		{math.MaxInt64/2 + 1, false}, // doubling this wraps to math.MinInt64
		{math.MaxInt64, false},
	} {
		if got := largeSizeWithinBudget(tc.size); got != tc.want {
			t.Errorf("largeSizeWithinBudget(%d) = %v, want %v", tc.size, got, tc.want)
		}
	}
}
