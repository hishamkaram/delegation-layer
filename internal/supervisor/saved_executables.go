package supervisor

import (
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ResolveSavedSupervisorClient preserves a recorded installation while present.
// Historical state copies select the installed role without opening the copy.
// This selects a transport only; the saved binding remains authoritative.
func ResolveSavedSupervisorClient(root string, saved task.SupervisorRef, lookup installation.ExecutableLookup) (string, error) {
	if err := task.ValidateSupervisorRef(saved); err != nil {
		return "", err
	}
	if !pueue.IsLegacyStateSupervisorClient(root, saved) && saved.ClientExecutable != "" {
		client, err := installation.ResolveSupervisorExecutable(saved.ClientExecutable, "pueue")
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return client, err
		}
	}
	return installation.ResolveInstalledSupervisorClient(lookup)
}
