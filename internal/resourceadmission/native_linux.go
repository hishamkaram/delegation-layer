//go:build linux

package resourceadmission

import (
	"context"
	"os"

	"github.com/shirou/gopsutil/v4/common"
	"github.com/shirou/gopsutil/v4/mem"
)

// Kernel file roots handed to gopsutil explicitly so an inherited HOST_PROC
// or HOST_SYS can never redirect a reading to a fake host.
const (
	procRoot = "/proc"
	sysRoot  = "/sys"
)

func kernelContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, common.EnvKey, common.EnvMap{
		common.HostProcEnvKey: procRoot,
		common.HostSysEnvKey:  sysRoot,
	})
}

func newNativeObserver() composedObserver {
	cgroups := cgroupReader{fsys: os.DirFS("/")}
	return composedObserver{
		targeted:   true,
		cpu:        readHostCPU,
		memory:     readHostMemory,
		cgroup:     cgroups.sample,
		cgroupPath: cgroups.membership,
	}
}

// readHostMemory reads /proc/meminfo. MemAvailable is the kernel's estimate
// of memory available without swapping.
func readHostMemory(ctx context.Context) reading[hostMemory] {
	vm, err := mem.VirtualMemoryWithContext(kernelContext(ctx))
	if err != nil || vm == nil {
		return unobserved[hostMemory](ReasonUnreadable)
	}
	return observed(hostMemory{source: memorySourceAvailable, total: vm.Total, available: vm.Available})
}
