//go:build !linux

package resourceadmission

// noCgroup and noCgroupPath serve platforms without cgroups, where the
// target-specific metrics do not exist and are never reported.
func noCgroup(*Target) reading[cgroupSample] { return notApplicable[cgroupSample]() }

func noCgroupPath(Target) reading[string] { return observed("") }
