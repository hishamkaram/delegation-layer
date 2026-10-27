//go:build linux

package resourceadmission

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// The fixtures stay well inside the 128MiB limit: 16MiB of clean file cache,
// 16MiB of anonymous memory, 8MiB of tmpfs and 8MiB of dirty file data on top
// of a baseline of a few tens of MiB. Nothing here reaches memory.max, so no
// reclaim, OOM kill or memory guarantee is being tested.
const (
	fixtureCacheBytes     = 16 << 20
	fixtureAnonBytes      = 16 << 20
	fixtureTmpfsBytes     = 8 << 20
	fixtureDirtyBytes     = 8 << 20
	fixtureBudgetBytes    = fixtureCacheBytes + fixtureAnonBytes + fixtureTmpfsBytes + fixtureDirtyBytes
	fixtureHeadroomBytes  = 32 << 20
	fixtureShmDir         = "/dev/shm"
	accountingSlackBytes  = 4 << 20
	pageBytes             = 4096
	fixtureWriteChunkSize = 1 << 20
)

// memoryAccount is one reading of the container cgroup's own memory files.
type memoryAccount struct {
	current, anon, file, shmem, fileDirty, inactiveFile uint64
	workingSet                                          uint64
	psi                                                 reading[uint64]
}

func readMemoryAccount(t *testing.T) memoryAccount {
	t.Helper()
	reader := cgroupReader{fsys: os.DirFS("/")}
	where, reason := reader.locate(containerTarget)
	if reason != "" {
		t.Fatalf("container cgroup not located: %s", reason)
	}
	var acct memoryAccount
	var err error
	if acct.current, err = reader.requiredUint(where.dir, "memory.current"); err != nil {
		t.Fatalf("memory.current: %v", err)
	}
	fields := map[string]*uint64{
		"anon": &acct.anon, "file": &acct.file, "shmem": &acct.shmem,
		"file_dirty": &acct.fileDirty, "inactive_file": &acct.inactiveFile,
	}
	for key, dst := range fields {
		if *dst, err = reader.requiredKeyed(where.dir, "memory.stat", key); err != nil {
			t.Fatalf("memory.stat %s: %v", key, err)
		}
	}
	levels := reader.levelMemory(where.dir)
	if !levels.ok {
		t.Fatalf("memory level unreadable: %s", levels.failure())
	}
	acct.workingSet = levels.value.workingSet
	acct.psi = reader.psi(where)
	return acct
}

func (a memoryAccount) log(t *testing.T, phase string) {
	t.Helper()
	t.Logf("%-12s current %d anon %d file %d shmem %d file_dirty %d inactive_file %d working-set estimate %d psi %+v",
		phase, a.current, a.anon, a.file, a.shmem, a.fileDirty, a.inactiveFile, a.workingSet, a.psi)
}

// fixtures own every file and allocation the test creates.
type fixtures struct {
	t    *testing.T
	anon []byte
	dirs []string
}

func newFixtures(t *testing.T) *fixtures {
	f := &fixtures{t: t}
	t.Cleanup(f.release)
	return f
}

func (f *fixtures) release() {
	f.anon = nil
	for _, dir := range f.dirs {
		if err := os.RemoveAll(dir); err != nil {
			f.t.Errorf("remove fixture %s: %v", dir, err)
		}
	}
	debug.FreeOSMemory()
}

// file writes size bytes of non-zero data under parent. A synced file leaves
// clean cache; an unsynced one leaves dirty pages until writeback.
func (f *fixtures) file(parent, name string, size int, sync bool) {
	f.t.Helper()
	dir, err := os.MkdirTemp(parent, "ra-native-")
	if err != nil {
		f.t.Fatalf("fixture directory under %q: %v", parent, err)
	}
	f.dirs = append(f.dirs, dir)
	out, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		f.t.Fatalf("create fixture: %v", err)
	}
	chunk := make([]byte, fixtureWriteChunkSize)
	for i := range chunk {
		chunk[i] = byte(i%251 + 1)
	}
	for written := 0; written < size; written += len(chunk) {
		if _, err = out.Write(chunk); err != nil {
			f.t.Fatalf("write fixture: %v", err)
		}
	}
	if sync {
		if err = out.Sync(); err != nil {
			f.t.Fatalf("sync fixture: %v", err)
		}
	}
	if err = out.Close(); err != nil {
		f.t.Fatalf("close fixture: %v", err)
	}
}

func (f *fixtures) allocate(size int) {
	f.anon = make([]byte, size)
	for i := 0; i < len(f.anon); i += pageBytes {
		f.anon[i] = 1
	}
	runtime.KeepAlive(f.anon)
}

func TestNativeContainerMemoryAccountingStaysBelowRefusalWithoutPSIStall(t *testing.T) {
	requireOwnedContainer(t)
	containerSample(t)
	base := readMemoryAccount(t)
	base.log(t, "baseline")
	if base.current+fixtureBudgetBytes > containerMemoryBytes-fixtureHeadroomBytes {
		t.Fatalf("baseline memory.current %d leaves no safe room for %d bytes of fixtures", base.current, fixtureBudgetBytes)
	}
	fix := newFixtures(t)
	fix.file(os.TempDir(), "cache", fixtureCacheBytes, true)
	cached := readMemoryAccount(t)
	cached.log(t, "clean cache")
	requireGrowth(t, "clean file cache", base.file, cached.file, fixtureCacheBytes)
	requireWorkingSetEstimate(t, cached)
	requireNoCgroupMemoryRefusal(t, "clean cache alone")

	fix.allocate(fixtureAnonBytes)
	anon := readMemoryAccount(t)
	anon.log(t, "anonymous")
	requireGrowth(t, "anonymous memory", cached.anon, anon.anon, fixtureAnonBytes)

	fix.file(fixtureShmDir, "tmpfs", fixtureTmpfsBytes, false)
	tmpfs := readMemoryAccount(t)
	tmpfs.log(t, "tmpfs")
	requireGrowth(t, "tmpfs shared memory", anon.shmem, tmpfs.shmem, fixtureTmpfsBytes)

	fix.file(os.TempDir(), "dirty", fixtureDirtyBytes, false)
	dirty := readMemoryAccount(t)
	dirty.log(t, "dirty data")
	requireWorkingSetEstimate(t, dirty)
	requireNoCgroupMemoryRefusal(t, "all fixtures held")
}

// requireGrowth checks that the kernel accounted at least most of the bytes
// the fixture added to one memory.stat category.
func requireGrowth(t *testing.T, what string, before, after uint64, added int) {
	t.Helper()
	if !accountedGrowth(before, after, uint64(added)) {
		t.Fatalf("%s went from %d to %d bytes, want growth of about %d", what, before, after, added)
	}
}

// accountedGrowth reports whether a category grew from before to after by at
// least added less the accounting slack. A decrease is never growth; the
// check runs before the unsigned subtraction so it cannot wrap.
func accountedGrowth(before, after, added uint64) bool {
	return after >= before && after-before+accountingSlackBytes >= added
}

// requireWorkingSetEstimate checks that the production estimate is the
// documented memory.current - inactive_file, read at nearly the same time,
// and is never presented as a hard-usage figure.
func requireWorkingSetEstimate(t *testing.T, acct memoryAccount) {
	t.Helper()
	later := readMemoryAccount(t)
	low, high := workingSet(min(acct.current, later.current), max(acct.inactiveFile, later.inactiveFile)),
		workingSet(max(acct.current, later.current), min(acct.inactiveFile, later.inactiveFile))
	if acct.workingSet+accountingSlackBytes < low || acct.workingSet > high+accountingSlackBytes {
		t.Fatalf("working-set estimate %d is outside current-inactive_file bounds [%d, %d]", acct.workingSet, low, high)
	}
	if acct.workingSet > acct.current {
		t.Fatalf("working-set estimate %d exceeds memory.current %d", acct.workingSet, acct.current)
	}
}

// requireNoCgroupMemoryRefusal runs a native Check. Moderate headroom use and
// cache with no PSI stall must not refuse on cgroup memory; a kernel without
// PSI must report only the missing-PSI warning.
func requireNoCgroupMemoryRefusal(t *testing.T, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), containerCheckWindow)
	defer cancel()
	got, err := NewChecker().Check(ctx, selfResolver{})
	if err != nil {
		t.Fatalf("%s: Check error = %v", when, err)
	}
	for _, p := range got.Pressures() {
		if p.Metric == MetricCgroupMemory {
			t.Fatalf("%s: refused on cgroup memory: %v", when, got.Pressures())
		}
	}
	requireOnlyMissingPSIWarning(t, got)
	psi := readMemoryAccount(t).psi
	missing := containsUnavailable(got.Unavailable(), Unavailable{Metric: MetricCgroupMemory, Reason: ReasonPressureUnavailable})
	if psi.ok == missing && !containsMetric(got.Pressures(), MetricHostMemory) {
		t.Fatalf("%s: psi readable %t but missing-PSI warning %t", when, psi.ok, missing)
	}
	t.Logf("%s: status %s pressures %v unavailable %v advisories %v", when, got.Status(), got.Pressures(), got.Unavailable(), got.Advisories())
}

func containsUnavailable(all []Unavailable, want Unavailable) bool {
	for _, u := range all {
		if u == want {
			return true
		}
	}
	return false
}
