//go:build darwin

package resourceadmission

import (
	"context"

	"golang.org/x/sys/unix"
)

func newNativeObserver() composedObserver {
	return composedObserver{
		cpu:        readHostCPU,
		memory:     readHostMemory,
		cgroup:     noCgroup,
		cgroupPath: noCgroupPath,
	}
}

// readHostMemory reads the kernel's VM pressure level: 1 normal, 2 warning,
// 4 critical. Available memory is not used on macOS.
func readHostMemory(context.Context) reading[hostMemory] {
	level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return unobserved[hostMemory](ReasonUnreadable)
	}
	return observed(hostMemory{source: memorySourceVMPressure, vm: vmPressure(level)})
}
