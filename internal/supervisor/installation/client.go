package installation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ResolveInstalledSupervisorClient selects only the installed client in the captured scope.
func ResolveInstalledSupervisorClient(lookup ExecutableLookup) (string, error) {
	if lookup.InitialExecutable != "" {
		return ResolveSupervisorExecutable(lookup.InitialExecutable, "pueue")
	}
	directory, err := bundledSupervisorDirectory(lookup.InitialExecutable)
	if err != nil {
		return "", err
	}
	client, err := ResolveSupervisorExecutable(filepath.Join(directory, "pueue"), "pueue")
	if err == nil || !errors.Is(err, os.ErrNotExist) || lookup.InitialExecutable != "" {
		return client, err
	}
	client, err = ResolveSupervisorExecutable(filepath.Join(directory, "..", "libexec", "pueue"), "pueue")
	if err == nil || !errors.Is(err, os.ErrNotExist) || lookup.Environment == nil {
		return client, err
	}
	candidate, err := lookup.lookPath("pueue")
	if err != nil {
		return "", fmt.Errorf("resolve captured installed pueue: %w", err)
	}
	return ResolveSupervisorExecutable(candidate, "pueue")
}
