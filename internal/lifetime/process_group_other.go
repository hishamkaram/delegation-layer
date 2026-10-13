//go:build !linux && !darwin

package lifetime

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
)

var (
	processSignalTerm            = syscall.Signal(15)
	processSignalKill            = syscall.Signal(9)
	errProcessNoSuchEntity error = ErrUnsupported
)

func configureProcessGroupCommand(*processGroupScope, *exec.Cmd) error {
	return ErrUnsupported
}

func configureProcessGroupOwner() error {
	return ErrUnsupported
}

func observeProcessIdentity(int) (bool, string, error) {
	return false, "", ErrUnsupported
}

func observeStartedProcessIdentity(int) (bool, string, error) {
	return false, "", ErrUnsupported
}

func processGroupID(int) (int, error) {
	return 0, fmt.Errorf("%w: process-group observation", ErrUnsupported)
}

func signalProcessGroup(int, syscall.Signal) error {
	return ErrUnsupported
}

func signalVerifiedProcessGroup(context.Context, *processGroupScope, int, []LaunchRef, syscall.Signal) error {
	return ErrUnsupported
}

func validateProcessGroupReferenceIdentifier(string) error { return ErrUnsupported }

func openProcessGroupPlatformState(context.Context, Ref) (string, processGroupPin, error) {
	return "", nil, ErrUnsupported
}

func bindProcessGroupPlatformState(*processGroupScope) error { return ErrUnsupported }

func processGroupPlatformReference(*processGroupScope) Ref { return Ref{} }

func validateProcessGroupLaunch(*processGroupScope, int, int) error { return ErrUnsupported }

func validateProcessGroupSignalTargets(*processGroupScope, map[int][]LaunchRef) error {
	return ErrUnsupported
}

func processGroupPlatformInfrastructure(*processGroupScope) []ProcessRef { return nil }

func releaseProcessGroupPlatformState(*processGroupScope) error { return nil }

func destroyProcessGroupPlatformState(*processGroupScope) error { return ErrUnsupported }

func processGroupExists(int) (bool, error) {
	return false, ErrUnsupported
}

func processGroupMembersPresent(int, []LaunchRef) (bool, error) {
	return false, ErrUnsupported
}

func discoverProcessScopeLaunches(context.Context, string, []LaunchRef, []LaunchRef, *ProcessRef, []ProcessRef, bool) ([]LaunchRef, error) {
	return nil, ErrUnsupported
}
