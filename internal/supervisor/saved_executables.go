package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ResolveSavedSupervisorClient preserves a recorded installation while present.
// Historical state copies select the installed role without opening the copy.
// This selects a transport only; the saved binding remains authoritative.
func ResolveSavedSupervisorClient(root string, saved task.SupervisorRef, lookup ExecutableLookup) (string, error) {
	if err := task.ValidateSupervisorRef(saved); err != nil {
		return "", err
	}
	if !pueue.IsLegacyStateSupervisorClient(root, saved) && saved.ClientExecutable != "" {
		client, err := resolveSupervisorExecutable(saved.ClientExecutable, "pueue")
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return client, err
		}
	}
	return resolveInstalledSupervisorClient(lookup)
}

// Read-only callers may use the captured launch PATH, never an ambient fallback.
func resolveInstalledSupervisorClient(lookup ExecutableLookup) (string, error) {
	if lookup.InitialExecutable != "" {
		return resolveSupervisorExecutable(lookup.InitialExecutable, "pueue")
	}
	directory, err := bundledSupervisorDirectory(lookup.InitialExecutable)
	if err != nil {
		return "", err
	}
	client, err := resolveSupervisorExecutable(filepath.Join(directory, "pueue"), "pueue")
	if err == nil || !errors.Is(err, os.ErrNotExist) || lookup.InitialExecutable != "" {
		return client, err
	}
	client, err = resolveSupervisorExecutable(filepath.Join(directory, "..", "libexec", "pueue"), "pueue")
	if err == nil || !errors.Is(err, os.ErrNotExist) || lookup.Environment == nil {
		return client, err
	}
	candidate, err := lookup.lookPath("pueue")
	if err != nil {
		return "", fmt.Errorf("resolve captured installed pueue: %w", err)
	}
	return resolveSupervisorExecutable(candidate, "pueue")
}
