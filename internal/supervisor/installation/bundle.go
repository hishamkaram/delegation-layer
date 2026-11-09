package installation

import (
	"errors"
	"os"
	"path/filepath"
)

// BundleRole is a metadata observation of one role in the selected installation.
// Err describes observation or validation failure; no executable is launched.
type BundleRole struct {
	Path string
	Err  error
}

// ObserveBundle selects only roles belonging to the loaded Delegate installation.
// It shares the runtime's colocated/libexec selection rule and never falls back
// to unrelated PATH tools. An incomplete colocated pair remains authoritative.
func ObserveBundle(self string) map[string]BundleRole {
	self = canonicalExecutablePath(self)
	directory := filepath.Dir(self)
	supervisorDirectory := selectBundledSupervisorDirectory(directory, false)
	paths := map[string]string{"delegate": self, "delegate_run": managedRunnerFromExecutable(self), "pueue": filepath.Join(supervisorDirectory, "pueue"), "pueued": filepath.Join(supervisorDirectory, "pueued")}
	roles := make(map[string]BundleRole, len(paths))
	for name, path := range paths {
		selected, err := ResolveSupervisorExecutable(path, name)
		if err == nil {
			path = selected
		}
		roles[name] = BundleRole{Path: path, Err: err}
	}
	return roles
}

// BundleRoleStatus distinguishes reliable absence/invalid metadata from I/O
// failures whose cause prevents a complete observation.
func BundleRoleStatus(err error) string {
	switch {
	case err == nil:
		return "available"
	case errors.Is(err, os.ErrNotExist):
		return "missing"
	case errors.Is(err, os.ErrPermission):
		return "observation_incomplete"
	default:
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return "observation_incomplete"
		}
		return "unusable"
	}
}
