//go:build !linux && !darwin

package resourceadmission

import "context"

// Other platforms build but are not supported: memory is reported
// unavailable instead of guessed.
func newNativeObserver() observer {
	return composedObserver{
		cpu:        readHostCPU,
		memory:     readHostMemory,
		cgroup:     noCgroup,
		cgroupPath: noCgroupPath,
	}
}

func readHostMemory(context.Context) reading[hostMemory] {
	return unobserved[hostMemory](ReasonUnsupportedPlatform)
}
