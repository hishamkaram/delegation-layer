package control

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// validateSavedPrivateProbeConfig reads the required saved config through the
// canonical resolver and isolation policy. Generation preparation alone owns
// creation of derived directories. Their absence is observed later, after the
// published generation and its record dependencies have been validated.
func validateSavedPrivateProbeConfig(stateRoot string, saved task.SupervisorRef, options Options) error {
	resolution, err := privateResolution(options)
	if err != nil {
		return err
	}
	if err = rejectSymlinkComponents(saved.ConfigPath); err != nil {
		return err
	}
	if _, err = os.Stat(saved.ConfigPath); err != nil {
		return fmt.Errorf("%w: saved private supervisor config is unavailable: %w", ErrBinding, err)
	}
	_, err = readPrivateConfig(saved.ConfigPath, filepath.Join(stateRoot, privateSupervisorDirectory), resolution)
	return err
}
