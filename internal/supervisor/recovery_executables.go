package supervisor

import (
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ResolveRecoveryPrivateSupervisorExecutables(root string, saved task.SupervisorRef, lookup installation.ExecutableLookup) (client, daemon string, resultErr error) {
	client, daemon, available, err := availableSavedPair(root, saved)
	if err != nil || available {
		return client, daemon, err
	}
	client, daemon, resolveErr := installation.ResolvePrivateSupervisorExecutables(lookup)
	if resolveErr == nil {
		return client, daemon, nil
	}
	return "", "", fmt.Errorf("%w: resolve private supervisor executables: %w", pueue.ErrBinding, resolveErr)
}

// availableSavedPair preserves recorded installation paths while present.
// Reserved historical copies are data, never executable candidates.
func availableSavedPair(root string, saved task.SupervisorRef) (client, daemon string, available bool, resultErr error) {
	if err := task.ValidateSupervisorRef(saved); err != nil {
		return "", "", false, err
	}
	if pueue.IsLegacyStateSupervisorClient(root, saved) || saved.ClientExecutable == "" || saved.DaemonExecutable == "" {
		return "", "", false, nil
	}
	client, clientErr := installation.ResolveSupervisorExecutable(saved.ClientExecutable, "pueue")
	daemon, daemonErr := installation.ResolveSupervisorExecutable(saved.DaemonExecutable, "pueued")
	if clientErr == nil && daemonErr == nil {
		return client, daemon, true, nil
	}
	for _, err := range []error{clientErr, daemonErr} {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", false, errors.Join(clientErr, daemonErr)
		}
	}
	// Only positive native absence reaches here; hard resolution faults above
	// remain errors rather than permission to choose another installation.
	return "", "", false, nil
}
