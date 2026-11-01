//go:build darwin

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
	observe := func(pid int) (bool, darwinProcessIdentity, error) {
		return inspectDarwinProcessIdentityWith(pid, nativeDarwinProcInfo, nativeDarwinProcessKinfoLookup)
	}
	if signal == syscall.SIGTERM {
		present, signalErr := signalDarwinProcessWith(ctx, launch, signal, observe, signalDarwinProcess)
		if signalErr == nil && !present {
			signalErr = ErrUnverifiableLaunch
		}
		return signalErr
	}
	// The production guardian deliberately permits only TERM/KILL. For this
	// test's INT/HUP use its exact identity check and native audit-token ABI,
	// keeping that production guard unchanged and never signalling a PGID.
	present, _, err := signalDarwinProcessOnce(ctx, launch, signal, observe, nativeSchedulerHostAuditSignal)
	if err == nil && !present {
		return ErrUnverifiableLaunch
	}
	return err
}

func nativeSchedulerHostAuditSignal(pid int, version int32, signal syscall.Signal) error {
	if signal != syscall.SIGINT && signal != syscall.SIGHUP {
		return fmt.Errorf("unsupported audit-token acceptance signal %d", signal)
	}
	token, err := darwinAuditToken(pid, version)
	if err != nil {
		return err
	}
	returned, err := nativeDarwinProcInfo(darwinProcInfoCallSignalAuditToken, 0, uint32(signal), 0, token)
	if err != nil {
		return err
	}
	if returned != 0 {
		return fmt.Errorf("%w: audit-token signal returned %d", ErrUnverifiableLaunch, returned)
	}
	return nil
}
