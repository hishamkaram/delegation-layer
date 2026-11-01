//go:build linux

package lifetime

import (
	"context"
	"fmt"
	"syscall"
)

func nativeSchedulerHostSignal(ctx context.Context, process ProcessRef, signal syscall.Signal) error {
	if signal != syscall.SIGINT && signal != syscall.SIGTERM && signal != syscall.SIGHUP {
		return fmt.Errorf("unsupported native scheduler acceptance signal %d", signal)
	}
	group, err := processGroupID(process.PID())
	if err != nil {
		return err
	}
	if group != process.PID() {
		return fmt.Errorf("native test host has no invocation-owned independent session")
	}
	launch, err := NewLaunchRef(process.PID(), process.StartToken(), group)
	if err != nil {
		return err
	}
	// Despite its historical name, this primitive opens verified pidfds and
	// signals only these exact identities. Never enumerate/broadcast the group.
	return signalVerifiedProcessGroup(ctx, nil, group, []LaunchRef{launch}, signal)
}
