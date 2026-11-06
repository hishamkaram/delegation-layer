//go:build linux

package lifetime

import (
	"context"
	"fmt"
	"os"
	"strings"
)

func checkNativeHostPlacement(ctx context.Context) error {
	data, err := os.ReadFile(procSelfCgroupPath)
	if err != nil {
		return fmt.Errorf("inspect current process cgroup: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return checkHostCgroupMembership(data)
}

func checkHostCgroupMembership(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("%w: current process cgroup observation is empty", ErrUnverifiableLaunch)
	}
	hasV2 := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			hasV2 = true
		}
	}
	if !hasV2 {
		// Delegate's exact backend uses v2; a v1-only host may still run the
		// cooperative backend. This observation certifies no outside placement.
		return nil
	}
	path, err := parseCgroupV2Path(data)
	if err != nil {
		return fmt.Errorf("inspect current process cgroup: %w", err)
	}
	for _, component := range strings.Split(path, "/") {
		if validateCgroupPlanName(component) == nil {
			return ErrTaskContained
		}
	}
	return nil
}
