package resourceadmission

import (
	"strconv"
	"testing"
	"testing/fstest"
)

const (
	psiTarget   = "some avg10=0.00 avg60=0.00 avg300=0.00 total=111\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n"
	psiAncestor = "some avg10=9.00 avg60=9.00 avg300=9.00 total=999999\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n"
)

func file(text string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(text)} }

func num(v uint64) string { return strconv.FormatUint(v, 10) + "\n" }

const stdMountinfo = "36 35 0:30 / /sys/fs/cgroup rw,nosuid - cgroup2 cgroup2 rw\n"

// treeAt builds a cgroup v2 mount at point (no leading slash): a limited
// ancestor "a" with an unlimited target "a/b" and a hot sibling "a/s". The
// mount root has no memory.max and a cpuset, like a real host root.
func treeAt(point string) fstest.MapFS {
	files := map[string]string{
		"cgroup.controllers":    "cpuset cpu memory\n",
		"cpuset.cpus.effective": "0-3\n",
		"cpu.stat":              "usage_usec 5000000\nuser_usec 1\n",
		"a/cgroup.events":       "populated 1\n",
		"a/memory.max":          num(2 * gib),
		"a/memory.high":         "max\n",
		"a/memory.current":      num(1500 << 20),
		"a/memory.stat":         "anon 1\ninactive_file " + num(500<<20),
		"a/memory.pressure":     psiAncestor,
		"a/cpu.max":             "200000 100000\n",
		"a/cpu.stat":            "usage_usec 900\n",
		"a/b/cgroup.events":     "populated 1\n",
		"a/b/memory.pressure":   psiTarget,
		"a/b/cpu.stat":          "usage_usec 1\n",
		"a/s/cgroup.events":     "populated 1\n",
		"a/s/memory.pressure":   psiAncestor,
	}
	tree := fstest.MapFS{}
	for name, text := range files {
		tree[point+"/"+name] = file(text)
	}
	return tree
}

// hostTree is a standard host: cgroup2 at /sys/fs/cgroup, process 4242 in
// /a/b.
func hostTree(membership string) fstest.MapFS {
	tree := treeAt("sys/fs/cgroup")
	tree[mountinfoFile] = file(stdMountinfo)
	tree["proc/4242/cgroup"] = file(membership)
	tree[selfCgroupFile] = file("0::/a/b\n")
	return tree
}

func readerFor(tree fstest.MapFS) cgroupReader { return cgroupReader{fsys: tree} }

func sampleOf(t *testing.T, reader cgroupReader, target *Target) cgroupSample {
	t.Helper()
	got := reader.sample(target)
	if !got.ok {
		t.Fatalf("sample unavailable: %s", got.failure())
	}
	return got.value
}

func TestSampleReadsTargetPSIFromTheTargetCgroupOnly(t *testing.T) {
	reader := readerFor(hostTree("0::/a/b\n"))
	sample := sampleOf(t, reader, &Target{Identity: "i", PID: 4242})
	if sample.path != "/sys/fs/cgroup/a/b" || len(sample.levels) != 3 {
		t.Fatalf("path = %q levels = %d, want /sys/fs/cgroup/a/b and 3 levels", sample.path, len(sample.levels))
	}
	if psi := sample.psiStallUS; !psi.ok || psi.value != 111 {
		t.Fatalf("psi = %+v, want the target's own 111, not the ancestor's or sibling's", psi)
	}
}

func TestSampleReadsMemoryLimitsAndWorkingSetEstimatePerLevel(t *testing.T) {
	reader := readerFor(hostTree("0::/a/b\n"))
	sample := sampleOf(t, reader, &Target{Identity: "i", PID: 4242})
	if mem := sample.levels[0].memory; !mem.ok || mem.value.max.set {
		t.Fatalf("target memory = %+v, want unlimited", mem)
	}
	ancestor := sample.levels[1].memory
	if !ancestor.ok || ancestor.value.max != (limit{bytes: 2 * gib, set: true}) || ancestor.value.workingSet != 1000<<20 {
		t.Fatalf("ancestor memory = %+v, want working-set estimate current-inactive_file", ancestor)
	}
	if ancestor.value.high.set {
		t.Fatalf("memory.high = %+v, want unset for max", ancestor.value.high)
	}
}

func TestSampleReadsCPUCapacityAndOwnUsagePerConstrainedLevel(t *testing.T) {
	reader := readerFor(hostTree("0::/a/b\n"))
	sample := sampleOf(t, reader, &Target{Identity: "i", PID: 4242})
	quota := sample.levels[1].cpu
	if !quota.ok || !quota.value.hasQuota || quota.value.quotaCPUs != 2 || quota.value.usageUS != 900 {
		t.Fatalf("ancestor cpu = %+v, want 2 cpus and the ancestor's own usage", quota)
	}
	root := sample.levels[2].cpu
	if !root.ok || root.value.cpusetCPUs != 4 || root.value.usageUS != 5_000_000 {
		t.Fatalf("root cpu = %+v, want cpuset 4 with usage", root)
	}
	if target := sample.levels[0].cpu; !target.ok || target.value.usageUS != 0 {
		t.Fatalf("unconstrained target cpu = %+v, want usage not read", target)
	}
}

func hostTreeWith(mutate func(fstest.MapFS)) cgroupReader {
	tree := hostTree("0::/a/b\n")
	mutate(tree)
	return readerFor(tree)
}

var pid4242 = &Target{Identity: "i", PID: 4242}

func TestSampleUnavailability(t *testing.T) {
	cases := []struct {
		name   string
		reader cgroupReader
		target *Target
		want   Reason
	}{
		{"no target", readerFor(hostTree("0::/a/b\n")), nil, ReasonTargetUnavailable},
		{"no cgroup2 mount", hostTreeWith(func(m fstest.MapFS) {
			m[mountinfoFile] = file("30 1 0:26 / /sys/fs/cgroup rw - tmpfs tmpfs rw\n")
		}), pid4242, ReasonScopeUnresolved},
		{"process gone", hostTreeWith(func(m fstest.MapFS) { delete(m, "proc/4242/cgroup") }), pid4242, ReasonScopeUnresolved},
		{"v1 membership only", readerFor(hostTree("3:memory:/a\n")), pid4242, ReasonCgroupUnsupported},
		{"removed cgroup", readerFor(hostTree("0::/a/b (deleted)\n")), pid4242, ReasonScopeUnresolved},
		{"cgroup directory missing", readerFor(hostTree("0::/a/gone\n")), pid4242, ReasonScopeUnresolved},
		{"path outside the mount", readerFor(hostTree("0::/../../other\n")), pid4242, ReasonScopeUnresolved},
		{"explicit path escapes", readerFor(hostTree("0::/a/b\n")), &Target{Identity: "i", CgroupPath: "/a/../.."}, ReasonScopeUnresolved},
		{"mountinfo unreadable", hostTreeWith(func(m fstest.MapFS) { delete(m, mountinfoFile) }), pid4242, ReasonCgroupUnsupported},
	}
	for _, tc := range cases {
		got := tc.reader.sample(tc.target)
		if got.ok || got.failure() != tc.want {
			t.Errorf("%s: reading = %+v, want unavailable %q", tc.name, got, tc.want)
		}
	}
}

func TestSampleUsesExplicitPathOnlyWithoutPID(t *testing.T) {
	tree := hostTree("0::/a/s\n")
	reader := readerFor(tree)
	byPath := reader.sample(&Target{Identity: "i", CgroupPath: "/a/b"})
	if !byPath.ok || byPath.value.path != "/sys/fs/cgroup/a/b" {
		t.Fatalf("by path = %+v", byPath)
	}
	byPID := reader.sample(&Target{Identity: "i", PID: 4242, CgroupPath: "/a/b"})
	if !byPID.ok || byPID.value.path != "/sys/fs/cgroup/a/s" {
		t.Fatalf("by pid = %+v, want the PID's membership to win", byPID)
	}
}

// containerTree is a private cgroup namespace: the process sees its own
// cgroup as "/" and the cgroup2 mount is rooted there.
func containerTree(mountpoint, mountRoot, membership string) fstest.MapFS {
	tree := treeAt(mountpoint)
	tree["proc/4242/cgroup"] = file(membership)
	tree[selfCgroupFile] = file(membership)
	tree[mountinfoFile] = file("40 30 0:31 " + mountRoot + " /" + mountpoint + " rw - cgroup2 cgroup2 rw\n")
	return tree
}

func TestSampleFollowsNonStandardMountPoints(t *testing.T) {
	tree := containerTree("mnt/odd cg", "/", "0::/a/b\n")
	tree[mountinfoFile] = file("40 30 0:31 / /mnt/odd\\040cg rw - cgroup2 cgroup2 rw\n")
	got := sampleOf(t, readerFor(tree), pid4242)
	if got.path != "/mnt/odd cg/a/b" || len(got.levels) != 3 || got.hiddenAncestors {
		t.Fatalf("sample = path %q levels %d hidden %v", got.path, len(got.levels), got.hiddenAncestors)
	}
	if ancestor := got.levels[1].memory; !ancestor.ok || ancestor.value.max.bytes != 2*gib {
		t.Fatalf("ancestor memory = %+v, want limits read from the discovered mount", ancestor)
	}
}

func TestSampleInACgroupNamespaceStopsAtTheNamespaceRoot(t *testing.T) {
	// The namespace root "/" is cgroup "a" of the real tree, mounted at the
	// standard path, so the target "/b" is one level below it.
	tree := containerTree("sys/fs/cgroup", "/", "0::/b\n")
	for name, f := range treeAt("sys/fs/cgroup") {
		tree[name] = f
	}
	tree["sys/fs/cgroup/b/memory.pressure"] = file(psiTarget)
	tree["sys/fs/cgroup/b/cpu.stat"] = file("usage_usec 1\n")
	got := sampleOf(t, readerFor(tree), pid4242)
	if got.path != "/sys/fs/cgroup/b" || len(got.levels) != 2 || got.hiddenAncestors {
		t.Fatalf("sample = path %q levels %d hidden %v, want target plus namespace root", got.path, len(got.levels), got.hiddenAncestors)
	}
}

func TestSampleWithASubtreeMountMarksHiddenAncestors(t *testing.T) {
	// Only cgroup "/a" is mounted, at /mnt/cg; its limit is the mount root's.
	tree := fstest.MapFS{
		"proc/4242/cgroup":         file("0::/a/b\n"),
		selfCgroupFile:             file("0::/a/b\n"),
		mountinfoFile:              file("40 30 0:31 /a /mnt/cg rw - cgroup2 cgroup2 rw\n"),
		"mnt/cg/memory.max":        file(num(2 * gib)),
		"mnt/cg/memory.current":    file(num(1500 << 20)),
		"mnt/cg/memory.stat":       file("inactive_file " + num(500<<20)),
		"mnt/cg/b/cpu.stat":        file("usage_usec 1\n"),
		"mnt/cg/b/memory.pressure": file(psiTarget),
	}
	got := sampleOf(t, readerFor(tree), pid4242)
	if !got.hiddenAncestors || len(got.levels) != 2 {
		t.Fatalf("sample = levels %d hidden %v, want the mount root a and target b with hidden ancestors", len(got.levels), got.hiddenAncestors)
	}
	if limited := got.levels[1].memory; !limited.ok || limited.value.max.bytes != 2*gib {
		t.Fatalf("mount root memory = %+v, want the subtree root's own limit", limited)
	}
}

func TestSampleRefusesScopesTheMountCannotPlace(t *testing.T) {
	cases := map[string]struct{ mountRoot, membership string }{
		"membership outside the namespace": {"/", "0::/../../init.scope\n"},
		"mount root is a sibling":          {"/other", "0::/a/b\n"},
		"mount root only a name prefix":    {"/a/bb", "0::/a/b\n"},
	}
	for name, tc := range cases {
		got := readerFor(containerTree("sys/fs/cgroup", tc.mountRoot, tc.membership)).sample(pid4242)
		if got.ok || got.failure() != ReasonScopeUnresolved {
			t.Errorf("%s: reading = %+v, want scope_unresolved", name, got)
		}
		if scope := readerFor(containerTree("sys/fs/cgroup", tc.mountRoot, tc.membership)).membership(*pid4242); scope.ok || scope.value != "" {
			t.Errorf("%s: membership = %+v, must not invent a scope", name, scope)
		}
	}
}

func TestSampleSkipsAnUnrelatedCgroup2MountAndUsesTheOneThatPlacesTheTarget(t *testing.T) {
	tree := hostTree("0::/a/b\n")
	tree[mountinfoFile] = file("50 30 0:40 /other /mnt/other rw - cgroup2 cgroup2 rw\n" + stdMountinfo)
	if got := readerFor(tree).sample(pid4242); !got.ok {
		t.Fatalf("sample unavailable: %s", got.failure())
	}
}

func TestHybridControllersOutsideV2AreUnknownNeverObserved(t *testing.T) {
	membership := "12:memory:/a/b\n4:cpu,cpuacct:/a/b\n3:cpuset:/a/b\n1:name=systemd:/a/b\n0::/a/b\n"
	got := sampleOf(t, readerFor(hostTree(membership)), pid4242)
	if got.psiStallUS.ok || got.psiStallUS.failure() != ReasonCgroupUnsupported {
		t.Fatalf("psi = %+v, want cgroup_unsupported", got.psiStallUS)
	}
	for _, level := range got.levels {
		if level.memory.ok || level.cpu.ok || level.memory.failure() != ReasonCgroupUnsupported || level.cpu.failure() != ReasonCgroupUnsupported {
			t.Fatalf("level %s = %+v, want both dimensions unsupported", level.path, level)
		}
	}
}

func TestOnlyTheControllersOutsideV2AreMarkedUnsupported(t *testing.T) {
	got := sampleOf(t, readerFor(hostTree("4:cpu,cpuacct:/a/b\n0::/a/b\n")), pid4242)
	if !got.levels[1].memory.ok || got.levels[1].cpu.ok {
		t.Fatalf("ancestor = %+v, want memory read from v2 and cpu unsupported", got.levels[1])
	}
}

func TestPSISourceDistinguishesRealRootNamespaceRootAndTarget(t *testing.T) {
	const hostPSI = "some avg10=0.00 avg60=0.00 avg300=0.00 total=777\n"
	realRoot := func() fstest.MapFS {
		tree := hostTree("0::/\n")
		tree[hostPressure] = file(hostPSI)
		return tree
	}
	t.Run("real root has no pressure file and uses host PSI", func(t *testing.T) {
		got := sampleOf(t, readerFor(realRoot()), pid4242)
		if !got.psiStallUS.ok || got.psiStallUS.value != 777 {
			t.Fatalf("psi = %+v, want /proc/pressure/memory", got.psiStallUS)
		}
	})
	t.Run("namespace root with its own pressure file keeps it", func(t *testing.T) {
		tree := realRoot()
		tree["sys/fs/cgroup/memory.pressure"] = file(psiTarget)
		tree["sys/fs/cgroup/cgroup.events"] = file("populated 1\n")
		if got := sampleOf(t, readerFor(tree), pid4242); got.psiStallUS.value != 111 {
			t.Fatalf("psi = %+v, want the cgroup's own 111", got.psiStallUS)
		}
	})
	t.Run("namespace root without pressure file is unavailable, not host PSI", func(t *testing.T) {
		tree := realRoot()
		tree["sys/fs/cgroup/cgroup.events"] = file("populated 1\n")
		got := sampleOf(t, readerFor(tree), pid4242)
		if got.psiStallUS.ok || got.psiStallUS.failure() != ReasonPressureUnavailable {
			t.Fatalf("psi = %+v, want pressure_unavailable", got.psiStallUS)
		}
	})
	t.Run("non-root target never falls back to host PSI", func(t *testing.T) {
		tree := hostTree("0::/a/b\n")
		tree[hostPressure] = file(hostPSI)
		delete(tree, "sys/fs/cgroup/a/b/memory.pressure")
		got := sampleOf(t, readerFor(tree), pid4242)
		if got.psiStallUS.ok {
			t.Fatalf("psi = %+v, want unavailable", got.psiStallUS)
		}
	})
}

type fileFault struct {
	name   string
	mutate func(fstest.MapFS)
	check  func(*testing.T, cgroupSample)
}

func faultPressureMissing(t *testing.T, s cgroupSample) {
	if s.psiStallUS.ok || s.psiStallUS.failure() != ReasonPressureUnavailable {
		t.Errorf("psi = %+v, want pressure_unavailable", s.psiStallUS)
	}
}

func faultPressureMalformed(t *testing.T, s cgroupSample) {
	if s.psiStallUS.failure() != ReasonMalformed {
		t.Errorf("psi = %+v, want malformed", s.psiStallUS)
	}
}

func faultOnlyMemoryMalformed(t *testing.T, s cgroupSample) {
	if s.levels[1].memory.ok || s.levels[1].memory.failure() != ReasonMalformed || !s.levels[1].cpu.ok {
		t.Errorf("ancestor = %+v, want only memory malformed", s.levels[1])
	}
}

func faultMemoryUnavailable(t *testing.T, s cgroupSample) {
	if s.levels[1].memory.ok {
		t.Errorf("memory = %+v, want unavailable", s.levels[1].memory)
	}
}

func faultOnlyCPUUnavailable(t *testing.T, s cgroupSample) {
	if s.levels[1].cpu.ok || !s.levels[1].memory.ok {
		t.Errorf("ancestor = %+v, want only cpu unavailable", s.levels[1])
	}
}

func faultWorkingSetZero(t *testing.T, s cgroupSample) {
	if m := s.levels[1].memory; !m.ok || m.value.workingSet != 0 {
		t.Errorf("memory = %+v, want working set 0", m)
	}
}

func faultHighOnly(t *testing.T, s cgroupSample) {
	if m := s.levels[1].memory; !m.ok || m.value.max.set || !m.value.high.set || m.value.workingSet != 1000<<20 {
		t.Errorf("memory = %+v, want high set with estimate", m)
	}
}

func TestSampleKeepsOtherMetricsWhenOneFileIsBad(t *testing.T) {
	cases := []fileFault{
		{"missing target pressure file", func(m fstest.MapFS) { delete(m, "sys/fs/cgroup/a/b/memory.pressure") }, faultPressureMissing},
		{"malformed pressure file", func(m fstest.MapFS) { m["sys/fs/cgroup/a/b/memory.pressure"] = file("garbage") }, faultPressureMalformed},
		{"malformed memory.max", func(m fstest.MapFS) { m["sys/fs/cgroup/a/memory.max"] = file("lots") }, faultOnlyMemoryMalformed},
		{"missing memory.current for a limited level", func(m fstest.MapFS) { delete(m, "sys/fs/cgroup/a/memory.current") }, faultMemoryUnavailable},
		{"missing cpu.stat for a quota level", func(m fstest.MapFS) { delete(m, "sys/fs/cgroup/a/cpu.stat") }, faultOnlyCPUUnavailable},
		{"inactive_file above current saturates", func(m fstest.MapFS) { m["sys/fs/cgroup/a/memory.current"] = file("10\n") }, faultWorkingSetZero},
		{"high only still reads the estimate", func(m fstest.MapFS) {
			delete(m, "sys/fs/cgroup/a/memory.max")
			m["sys/fs/cgroup/a/memory.high"] = file(num(gib))
		}, faultHighOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree := hostTree("0::/a/b\n")
			tc.mutate(tree)
			tc.check(t, sampleOf(t, readerFor(tree), pid4242))
		})
	}
}

func TestMembershipReadsCurrentPath(t *testing.T) {
	reader := readerFor(hostTree("0::/a/s\n"))
	if got := reader.membership(Target{Identity: "i", PID: 4242}); !got.ok || got.value != "/sys/fs/cgroup/a/s" {
		t.Fatalf("membership = %+v", got)
	}
	if got := reader.membership(Target{Identity: "i", CgroupPath: "/a/b"}); !got.ok || got.value != "/sys/fs/cgroup/a/b" {
		t.Fatalf("explicit membership = %+v", got)
	}
	if got := reader.membership(Target{Identity: "i", PID: 1}); got.ok {
		t.Fatalf("membership = %+v, want unavailable for a missing process", got)
	}
	if got := reader.membership(Target{Identity: "i", CgroupPath: "/a/gone"}); got.ok || got.value != "" {
		t.Fatalf("membership = %+v, must not invent a scope for a missing cgroup", got)
	}
}
