//go:build linux

package resourceadmission

import (
	"errors"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// cgroupReader reads cgroup v2 state through one file tree rooted at the
// filesystem root: /proc for placement and mounts, and the cgroup2 mount
// wherever mountinfo says it is. It is stateless.
type cgroupReader struct {
	fsys fs.FS
}

const (
	mountinfoFile  = "proc/self/mountinfo"
	selfCgroupFile = "proc/self/cgroup"
	hostPressure   = "proc/pressure/memory"
	nonRootMarker  = "cgroup.events"
	memoryPressure = "memory.pressure"
)

// reasonOf maps a read failure to a stable reason without exposing its text.
func reasonOf(err error) Reason {
	if errors.Is(err, errMalformed) {
		return ReasonMalformed
	}
	return ReasonUnreadable
}

// maxMetadataBytes is the 1 MiB engineering per-file read budget for kernel
// metadata. Larger metadata is read as unknown and produces a warning.
const maxMetadataBytes = 1 << 20

// maxObservedAncestors bounds the cgroup levels read per sample. Deeper
// hierarchies are reported as incomplete coverage, never as complete.
const maxObservedAncestors = 128

// readText returns a file's text, read-only and bounded by maxMetadataBytes.
// present is false only when Open reports the file absent, which is how a
// disabled controller shows. A read, overflow or close fault is an error and
// no partial text is returned, so truncated data is never parsed.
func readText(fsys fs.FS, name string) (text string, present bool, err error) {
	f, err := fsys.Open(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	data, readErr := task.ReadBounded(f, maxMetadataBytes)
	if err = errors.Join(readErr, f.Close()); err != nil {
		return "", true, err
	}
	return string(data), true, nil
}

// located is a target cgroup placed on the filesystem.
type located struct {
	// path is the identity of the cgroup: its directory as resolved by the
	// canonical lifetime resolver, relative to the filesystem root.
	path string
	// dir and top are the cgroup directory and the mount point directory,
	// relative to the filesystem root.
	dir           string
	top           string
	hidden        bool
	memoryOutside bool
	cpuOutside    bool
}

// locate resolves the target's cgroup and its mount with the canonical
// lifetime resolver. A PID takes precedence over an explicit CgroupPath.
// Controllers that live in a v1 hierarchy are reported, not read.
func (g cgroupReader) locate(target Target) (located, Reason) {
	source := selfCgroupFile
	if target.PID > 0 {
		source = path.Join("proc", strconv.Itoa(target.PID), "cgroup")
	}
	text, present, err := readText(g.fsys, source)
	switch {
	case err != nil:
		return located{}, ReasonUnreadable
	case !present:
		return located{}, ReasonScopeUnresolved
	}
	place, reason := parsePlacement(text)
	if reason != "" {
		return located{}, reason
	}
	membership := []byte(text)
	if target.PID > 0 {
		if place.deleted {
			return located{}, ReasonScopeUnresolved
		}
	} else {
		membership = []byte("0::" + target.CgroupPath + "\n")
	}
	mounts, present, err := readText(g.fsys, mountinfoFile)
	switch {
	case err != nil:
		return located{}, ReasonUnreadable
	case !present:
		return located{}, ReasonCgroupUnsupported
	}
	location, err := lifetime.ResolveCgroupV2Location(membership, []byte(mounts))
	if err != nil {
		return located{}, ReasonScopeUnresolved
	}
	return g.place(location, place)
}

// place checks that the resolved directory exists on the filesystem.
func (g cgroupReader) place(location lifetime.CgroupV2Location, place placement) (located, Reason) {
	dir := strings.TrimPrefix(location.CurrentPath(), "/")
	top := strings.TrimPrefix(location.MountPath(), "/")
	if top == "" {
		top = "."
	}
	if dir == "" {
		dir = "."
	}
	if !fs.ValidPath(dir) || !fs.ValidPath(top) {
		return located{}, ReasonScopeUnresolved
	}
	if _, err := fs.Stat(g.fsys, dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return located{}, ReasonScopeUnresolved
		}
		return located{}, ReasonUnreadable
	}
	return located{
		path: location.CurrentPath(), dir: dir, top: top, hidden: location.MountRoot() != "/",
		memoryOutside: place.memoryOutside, cpuOutside: place.cpuOutside,
	}, ""
}

// membership resolves the current cgroup identity of target, empty and
// unavailable whenever sample could not place it.
func (g cgroupReader) membership(target Target) reading[string] {
	where, reason := g.locate(target)
	if reason != "" {
		return unobserved[string](reason)
	}
	return observed(where.path)
}

// sample reads the target cgroup, its memory PSI and every visible level up
// to the mount root.
func (g cgroupReader) sample(target *Target) reading[cgroupSample] {
	if target == nil {
		return unobserved[cgroupSample](ReasonTargetUnavailable)
	}
	where, reason := g.locate(*target)
	if reason != "" {
		return unobserved[cgroupSample](reason)
	}
	dirs, complete := chain(where.dir, where.top, maxObservedAncestors)
	levels := make([]cgroupLevel, len(dirs))
	for i, levelDir := range dirs {
		levels[i] = cgroupLevel{path: levelDir, memory: g.levelMemory(levelDir), cpu: g.levelCPU(levelDir)}
		if where.memoryOutside {
			levels[i].memory = unobserved[levelMemory](ReasonCgroupUnsupported)
		}
		if where.cpuOutside {
			levels[i].cpu = unobserved[levelCPU](ReasonCgroupUnsupported)
		}
	}
	return observed(cgroupSample{path: where.path, psiStallUS: g.psi(where), levels: levels, hiddenAncestors: where.hidden || !complete})
}

// psi reads the target cgroup's own memory.pressure. Only the real cgroup
// root, which has no pressure file and no cgroup.events, uses the host-wide
// /proc/pressure/memory; a namespace root is a real cgroup and keeps its own.
func (g cgroupReader) psi(where located) reading[uint64] {
	if where.memoryOutside {
		return unobserved[uint64](ReasonCgroupUnsupported)
	}
	text, present, err := readText(g.fsys, path.Join(where.dir, memoryPressure))
	if err == nil && !present && g.isRealRoot(where) {
		text, present, err = readText(g.fsys, hostPressure)
	}
	if err != nil || !present {
		return unobserved[uint64](ReasonPressureUnavailable)
	}
	total, err := parsePSISomeTotal(text)
	if err != nil {
		return unobserved[uint64](ReasonMalformed)
	}
	return observed(total)
}

func (g cgroupReader) isRealRoot(where located) bool {
	if where.hidden || where.dir != where.top {
		return false
	}
	_, nonRoot, err := readText(g.fsys, path.Join(where.dir, nonRootMarker))
	return err == nil && !nonRoot
}

// optionalLimit reads a memory limit file; an absent file means no limit.
func (g cgroupReader) optionalLimit(dir, name string) (limit, error) {
	text, present, err := readText(g.fsys, path.Join(dir, name))
	if err != nil || !present {
		return limit{}, err
	}
	return parseLimit(text)
}

func (g cgroupReader) requiredUint(dir, name string) (uint64, error) {
	text, present, err := readText(g.fsys, path.Join(dir, name))
	switch {
	case err != nil:
		return 0, err
	case !present:
		return 0, fs.ErrNotExist
	}
	return parseUint(text)
}

func (g cgroupReader) requiredKeyed(dir, name, key string) (uint64, error) {
	text, present, err := readText(g.fsys, path.Join(dir, name))
	switch {
	case err != nil:
		return 0, err
	case !present:
		return 0, fs.ErrNotExist
	}
	return keyedValue(text, key)
}

// levelMemory reads the limits of one level and, only when a limit is set,
// the working-set estimate memory.current minus inactive_file.
func (g cgroupReader) levelMemory(dir string) reading[levelMemory] {
	maxLimit, err := g.optionalLimit(dir, "memory.max")
	if err != nil {
		return unobserved[levelMemory](reasonOf(err))
	}
	highLimit, err := g.optionalLimit(dir, "memory.high")
	if err != nil {
		return unobserved[levelMemory](reasonOf(err))
	}
	if !maxLimit.set && !highLimit.set {
		return observed(levelMemory{})
	}
	current, err := g.requiredUint(dir, "memory.current")
	if err != nil {
		return unobserved[levelMemory](reasonOf(err))
	}
	inactive, err := g.requiredKeyed(dir, "memory.stat", "inactive_file")
	if err != nil {
		return unobserved[levelMemory](reasonOf(err))
	}
	return observed(levelMemory{max: maxLimit, high: highLimit, workingSet: workingSet(current, inactive)})
}

// workingSet is the estimate memory.current - inactive_file, saturating at
// zero. It is an estimate only: inactive file pages are not guaranteed
// reclaimable.
func workingSet(current, inactiveFile uint64) uint64 {
	if inactiveFile >= current {
		return 0
	}
	return current - inactiveFile
}

// levelCPU reads one level's quota and cpuset. Usage is read only for a
// constrained level; an absent file means that controller is not enabled.
func (g cgroupReader) levelCPU(dir string) reading[levelCPU] {
	var cpuLevel levelCPU
	maxText, present, err := readText(g.fsys, path.Join(dir, "cpu.max"))
	if err != nil {
		return unobserved[levelCPU](ReasonUnreadable)
	}
	if present {
		if cpuLevel.quotaCPUs, cpuLevel.hasQuota, err = parseCPUMax(maxText); err != nil {
			return unobserved[levelCPU](ReasonMalformed)
		}
	}
	setText, present, err := readText(g.fsys, path.Join(dir, "cpuset.cpus.effective"))
	if err != nil {
		return unobserved[levelCPU](ReasonUnreadable)
	}
	if present {
		if cpuLevel.cpusetCPUs, err = parseCPUSet(setText); err != nil {
			return unobserved[levelCPU](ReasonMalformed)
		}
	}
	if _, constrained := cpuCapacity(cpuLevel); !constrained {
		return observed(cpuLevel)
	}
	if cpuLevel.usageUS, err = g.requiredKeyed(dir, "cpu.stat", "usage_usec"); err != nil {
		return unobserved[levelCPU](reasonOf(err))
	}
	return observed(cpuLevel)
}
