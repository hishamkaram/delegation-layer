//go:build linux

package lifetime

// CgroupV2Location is one immutable cgroup v2 membership resolved by the
// canonical lifetime resolver. It carries paths only; it grants no scope,
// process, or control authority.
type CgroupV2Location struct {
	mountRoot   string
	mountPath   string
	currentPath string
}

// MountRoot returns the cgroup v2 hierarchy path that the selected mount
// exposes at MountPath.
func (l CgroupV2Location) MountRoot() string { return l.mountRoot }

// MountPath returns the selected cgroup v2 mount point.
func (l CgroupV2Location) MountPath() string { return l.mountPath }

// CurrentPath returns the filesystem path of the membership inside MountPath.
func (l CgroupV2Location) CurrentPath() string { return l.currentPath }

// ResolveCgroupV2Location resolves raw /proc/<pid>/cgroup bytes against raw
// mountinfo bytes with the same rule lifetime uses for its own scopes. It
// performs no I/O. Both inputs must come from one observer's cgroup and mount
// namespace view, for example /proc/<pid>/cgroup and /proc/self/mountinfo read
// by the same process. A membership outside that view, which the kernel
// prints with leading ".." components, is an error, never a nearby path. Any
// failure returns the zero location.
func ResolveCgroupV2Location(cgroupData, mountInfoData []byte) (CgroupV2Location, error) {
	location, err := resolveCgroupLocation(cgroupData, mountInfoData)
	if err != nil {
		return CgroupV2Location{}, err
	}
	return CgroupV2Location(location), nil
}
